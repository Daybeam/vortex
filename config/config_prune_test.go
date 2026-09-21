package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPruneStale_DirectUnit is a minimal unit test of the helper itself.
func TestPruneStale_DirectUnit(t *testing.T) {
	m := map[string]int{"a": 1, "b": 2, "c": 3}
	pruneStale(m, map[string]bool{"a": true, "c": true})
	if _, ok := m["b"]; ok {
		t.Fatalf("expected 'b' to be pruned, still present: %v", m)
	}
	if len(m) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d: %v", len(m), m)
	}
}

// TestLoad_RemovesRoleGroupDeletedFromDisk is a direct regression test for
// the 2026-08-03 incident: unregister_group returned {"ok": true} and the
// per-item split-layout file was even deleted from disk, but the group kept
// reappearing in get_config/query_groups across repeated config.reload()
// calls, because Registry.Load() only ever upserted into r.RoleGroups and
// never removed an entry whose file had disappeared. This test reproduces
// the exact repro steps: register (write+load), delete the file, reload,
// assert the group is actually gone from memory.
func TestLoad_RemovesRoleGroupDeletedFromDisk(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config_test.json")
	groupsDir := filepath.Join(dir, "workspace", "role_groups")
	if err := os.MkdirAll(groupsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Minimal split-layout main config pointing at the groups dir.
	main := map[string]any{
		"_split_layout": map[string]any{
			"version":     2,
			"role_groups": "workspace/role_groups/",
		},
	}
	mainBytes, _ := json.Marshal(main)
	if err := os.WriteFile(configPath, mainBytes, 0o644); err != nil {
		t.Fatalf("write main config: %v", err)
	}

	groupFile := filepath.Join(groupsDir, "probe_group.json")
	groupBytes := []byte(`{"id":"probe_group","name":"Probe","policy":"sequential"}`)
	if err := os.WriteFile(groupFile, groupBytes, 0o644); err != nil {
		t.Fatalf("write group file: %v", err)
	}

	r, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("initial Load: %v", err)
	}
	if _, ok := r.RoleGroups["probe_group"]; !ok {
		t.Fatalf("expected probe_group to be loaded, got: %v", r.RoleGroups)
	}

	// Simulate "unregister": delete the on-disk file, then reload -- this is
	// exactly what config.reload() does live.
	if err := os.Remove(groupFile); err != nil {
		t.Fatalf("remove group file: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("reload after delete: %v", err)
	}

	if _, ok := r.RoleGroups["probe_group"]; ok {
		t.Fatalf("BUG REPRODUCED: probe_group still present in r.RoleGroups after its file was deleted and Load() was called again: %v", r.RoleGroups)
	}
}

// TestLoad_DynamicMCPsSurviveReload confirms the fix does not touch
// r.DynamicMCPs, which has no on-disk source and must not be pruned by a
// disk-driven Load() call.
func TestLoad_DynamicMCPsSurviveReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config_test.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write main config: %v", err)
	}

	r, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("initial Load: %v", err)
	}
	r.DynamicMCPs["runtime_only"] = &MCPDef{ID: "runtime_only"}

	if err := r.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, ok := r.DynamicMCPs["runtime_only"]; !ok {
		t.Fatalf("r.DynamicMCPs entry was incorrectly pruned by a disk-driven Load() call")
	}
}

// TestPersist_PrunesOrphanedFiles verifies that Persist() in split-layout mode
// actually deletes the per-item file from disk when an entry is removed from
// the in-memory registry. Added 2026-08-08 fixing the "resurrecting deleted
// items" bug.
func TestPersist_PrunesOrphanedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// 1. Start with a whole config
	initialCfg := Config{
		RoleGroups: []RoleGroup{
			{ID: "group1", Name: "Group 1"},
			{ID: "group2", Name: "Group 2"},
		},
	}

	data, _ := json.Marshal(initialCfg)
	_ = os.WriteFile(configPath, data, 0644)

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	// Force split layout
	sl := &SplitLayout{
		Version:    2,
		RoleGroups: "role_groups/",
	}
	reg.setSplitLayout(sl)

	// Persist to create the split files
	if err := reg.Persist(); err != nil {
		t.Fatalf("first persist failed: %v", err)
	}

	// Verify files created
	group1Path := filepath.Join(tmpDir, "role_groups", "group1.json")
	group2Path := filepath.Join(tmpDir, "role_groups", "group2.json")

	if _, err := os.Stat(group1Path); err != nil {
		t.Errorf("group1.json not created")
	}
	if _, err := os.Stat(group2Path); err != nil {
		t.Errorf("group2.json not created")
	}

	// 2. Unregister group1 and persist
	reg.Mu.Lock()
	delete(reg.RoleGroups, "group1")
	reg.Mu.Unlock()

	if err := reg.Persist(); err != nil {
		t.Fatalf("failed to persist after unregister: %v", err)
	}

	// 3. Verify group1.json is DELETED and group2.json remains
	if _, err := os.Stat(group1Path); !os.IsNotExist(err) {
		t.Errorf("group1.json still exists after unregistering and persist")
	}
	if _, err := os.Stat(group2Path); err != nil {
		t.Errorf("group2.json was accidentally deleted")
	}

	// 4. Unregister the last group and persist
	reg.Mu.Lock()
	delete(reg.RoleGroups, "group2")
	reg.Mu.Unlock()

	if err := reg.Persist(); err != nil {
		t.Fatalf("failed to persist after unregistering last: %v", err)
	}

	if _, err := os.Stat(group2Path); !os.IsNotExist(err) {
		t.Errorf("group2.json still exists after unregistering last item")
	}
}
