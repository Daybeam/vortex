package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newSplitTestRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config_test.json")
	r, err := NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func addTestMCPs(r *Registry, n int) {
	r.Mu.Lock()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("mcp_%d", i)
		r.MCPs[id] = &MCPDef{ID: id, URL: "https://example.com/" + id, Trusted: true}
	}
	r.Mu.Unlock()
}
func TestPersist_SmallConfigStaysWhole(t *testing.T) {
	r := newSplitTestRegistry(t)
	if err := r.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	data, err := os.ReadFile(r.GetConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.SplitLayout != nil {
		t.Fatalf("expected no split layout for a small config, got %+v", cfg.SplitLayout)
	}
	dir := filepath.Dir(r.GetConfigPath())
	if _, err := os.Stat(filepath.Join(dir, "mcps.json")); err == nil {
		t.Fatalf("mcps.json should not exist for a small config")
	}
}
func TestPersist_LargeConfigAutoSplits(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)

	if err := r.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	dir := filepath.Dir(r.GetConfigPath())
	// Default split layout now writes each MCP to its own file inside
	// workspace/mcps/ (directory form), not a single flat mcps.json.
	mcpsDir := filepath.Join(dir, "workspace", "mcps")
	info, err := os.Stat(mcpsDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("expected workspace/mcps/ directory to exist after auto-split: %v", err)
	}
	data, err := os.ReadFile(r.GetConfigPath())
	if err != nil {
		t.Fatalf("read main config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.SplitLayout == nil || cfg.SplitLayout.MCPs != "workspace/mcps/" {
		t.Fatalf("expected split layout pointing at workspace/mcps/, got %+v", cfg.SplitLayout)
	}
	if len(cfg.MCPs) != 0 {
		t.Fatalf("expected main config MCPs cleared after split, got %d", len(cfg.MCPs))
	}
	entries, err := os.ReadDir(mcpsDir)
	if err != nil {
		t.Fatalf("read workspace/mcps/: %v", err)
	}
	if len(entries) != 20 {
		t.Fatalf("expected 20 mcp files in split directory, got %d", len(entries))
	}
}
func TestLoad_TransparentlyMergesSplitFiles(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)
	if err := r.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	r2, err := NewRegistry(r.GetConfigPath())
	if err != nil {
		t.Fatalf("NewRegistry (reload): %v", err)
	}
	r2.Mu.RLock()
	defer r2.Mu.RUnlock()
	if len(r2.MCPs) != 20 {
		t.Fatalf("expected reload to merge split mcps.json, got %d entries", len(r2.MCPs))
	}
}
func TestPersist_NoAutoSplitEnvDisables(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	t.Setenv("VORTEX_CONFIG_NO_AUTOSPLIT", "1")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)
	if err := r.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	dir := filepath.Dir(r.GetConfigPath())
	if _, err := os.Stat(filepath.Join(dir, "mcps.json")); err == nil {
		t.Fatalf("mcps.json should not exist when auto-split is disabled")
	}
}
func TestPersist_OnceSplitStaysSplit(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)
	if err := r.Persist(); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	r.Mu.Lock()
	for id := range r.MCPs {
		delete(r.MCPs, id)
	}
	r.Mu.Unlock()
	if err := r.Persist(); err != nil {
		t.Fatalf("second Persist: %v", err)
	}
	dir := filepath.Dir(r.GetConfigPath())
	// writeSection skips writing (and does not delete) when the in-memory
	// slice is empty, so the workspace/mcps/ directory from the first
	// Persist() should still be present after shrinking to zero MCPs.
	if info, err := os.Stat(filepath.Join(dir, "workspace", "mcps")); err != nil || !info.IsDir() {
		t.Fatalf("expected workspace/mcps/ directory to still exist after shrinking: %v", err)
	}
	data, err := os.ReadFile(r.GetConfigPath())
	if err != nil {
		t.Fatalf("read main config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.SplitLayout == nil {
		t.Fatalf("expected split layout to persist after shrinking below threshold")
	}
}
func TestAtomicWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := atomicWriteFile(p, []byte("first"), 0644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := atomicWriteFile(p, []byte("second"), 0644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("expected %q, got %q", "second", string(data))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "f.txt" {
			t.Fatalf("unexpected leftover file: %s", e.Name())
		}
	}
}
func TestDiagSplitThreshold(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	t.Logf("threshold=%d", splitThresholdBytes())
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)
	r.Mu.RLock()
	mcps := make([]MCPDef, 0, len(r.MCPs))
	for _, m := range r.MCPs {
		mcps = append(mcps, *m)
	}
	r.Mu.RUnlock()
	cfg := Config{MCPs: mcps}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	t.Logf("size=%d", len(b))
	t.Logf("splitLayout=%v", r.getSplitLayout())
}
func TestDiagSplitThreshold2(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)
	t.Logf("before Persist: env=%s existing=%v", os.Getenv("VORTEX_CONFIG_SPLIT_THRESHOLD"), r.getSplitLayout())
	err := r.Persist()
	t.Logf("Persist err=%v", err)
	t.Logf("after Persist: existing=%v", r.getSplitLayout())
	data, _ := os.ReadFile(r.GetConfigPath())
	t.Logf("mainfile len=%d", len(data))
}

func TestPersist_UnregisteredItem_FileRemovedOnDisk(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_SPLIT_THRESHOLD", "500")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 20)
	if err := r.Persist(); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	dir := filepath.Dir(r.GetConfigPath())
	mcpsDir := filepath.Join(dir, "workspace", "mcps")
	if _, err := os.Stat(filepath.Join(mcpsDir, "mcp_5.json")); err != nil {
		t.Fatalf("expected mcp_5.json to exist after first Persist: %v", err)
	}

	// Simulate unregister_mcp: remove one entry from the in-memory map.
	r.Mu.Lock()
	delete(r.MCPs, "mcp_5")
	r.Mu.Unlock()
	if err := r.Persist(); err != nil {
		t.Fatalf("second Persist: %v", err)
	}

	// FIX (2026-08-08) regression: the orphaned per-item file must actually
	// be deleted from disk, not just absent from the in-memory map.
	if _, err := os.Stat(filepath.Join(mcpsDir, "mcp_5.json")); err == nil {
		t.Fatalf("mcp_5.json should have been pruned from disk after unregister+Persist")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}

	// Remaining 19 files should be untouched.
	entries, err := os.ReadDir(mcpsDir)
	if err != nil {
		t.Fatalf("read mcps dir: %v", err)
	}
	if len(entries) != 19 {
		t.Fatalf("expected 19 remaining mcp files, got %d", len(entries))
	}

	// Restart-not-reload: a fresh Registry built from the same config path
	// must not resurrect the pruned entry (this is the actual restart
	// scenario flagged as unverified in the 2026-08-07 addendum SS6).
	r2, err := NewRegistry(r.GetConfigPath())
	if err != nil {
		t.Fatalf("NewRegistry (simulated restart): %v", err)
	}
	r2.Mu.RLock()
	defer r2.Mu.RUnlock()
	if _, stillThere := r2.MCPs["mcp_5"]; stillThere {
		t.Fatalf("mcp_5 resurfaced after simulated restart -- orphan file was not really pruned")
	}
	if len(r2.MCPs) != 19 {
		t.Fatalf("expected 19 mcps after restart, got %d", len(r2.MCPs))
	}
}
