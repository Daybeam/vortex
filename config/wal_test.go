package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfig_CorruptMainFile_QuarantineAndFallback(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"good"}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	// First startup: should load fine
	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("First NewRegistry failed: %v", err)
	}
	_ = reg

	// Now corrupt the main config file
	corruptData := []byte(`{"default_provider":"good", "providers": [ BROKEN JSON`)
	os.WriteFile(configPath, corruptData, 0644)

	// Second startup: should quarantine and bootstrap defaults, not crash
	reg2, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry should survive corrupted config, got: %v", err)
	}
	_ = reg2

	// Verify quarantine
	badPath := configPath + ".bad"
	if _, err := os.Stat(badPath); os.IsNotExist(err) {
		t.Error("expected quarantined .bad file to exist after corruption")
	}
}

func TestConfig_CorruptMainFile_RestoreFromBackup(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"original"}`), 0644)

	// Create a fresh .bak (simulating Compactor's last Persist)
	os.WriteFile(configPath+".bak", []byte(`{"default_provider":"original"}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	// Startup 1: Load OK, auto-saves .bak copy inside loadWithFallback
	reg1, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("First NewRegistry failed: %v", err)
	}
	_ = reg1

	// Verify .bak was created by loadWithFallback pre-load backup
	if _, err := os.Stat(configPath + ".bak"); os.IsNotExist(err) {
		t.Error("expected .bak to be created by loadWithFallback")
	}

	// Corrupt main config
	os.WriteFile(configPath, []byte(`CORRUPTED`), 0644)

	// Startup 2: should restore from .bak
	reg2, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry should restore from backup, got: %v", err)
	}
	_ = reg2

	// .bak restored → main config no longer corrupted
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read restored config: %v", err)
	}
	if !strings.Contains(string(data), "original") {
		t.Errorf("expected restored config to contain 'original', got: %s", string(data))
	}
}

func TestWAL_QuarantineOnReplayFailure(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"baseline"}`), 0644)
	walPath := configPath + ".wal"

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	// Step 1: Create a valid WAL entry
	wal := NewWAL(walPath)
	ops := []PatchOp{{Op: OpReplace, Path: "/default_provider", Value: json.RawMessage(`"modified"`)}}
	_, err := wal.Append("actor", "task", ops)
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// Step 2: Corrupt the WAL file by appending garbage
	corruptData := []byte("\nTHIS_IS_GARBAGE_NOT_JSON\n")
	if err := os.WriteFile(walPath, corruptData, 0644); err != nil {
		t.Fatalf("Write corrupt data failed: %v", err)
	}

	// Step 3: Start Registry — should quarantine corrupted WAL and fall back
	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry should not return error on WAL corruption, got: %v", err)
	}

	// Step 4: Verify WAL is quarantined (the actual safety guarantee)
	badPath := walPath + ".bad"
	if _, err := os.Stat(badPath); os.IsNotExist(err) {
		t.Error("expected quarantined WAL file .bad to exist")
	}

	// Step 5: Registry should still start (may have bootstrap defaults)
	_ = reg // must not be nil — verified by successful NewRegistry return
}

func TestWAL_CompactorOnlyOnSuccessfulReplay(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"p1"}`), 0644)
	walPath := configPath + ".wal"

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	// Step 1: Corrupt WAL before any startup
	os.WriteFile(walPath, []byte("CORRUPT_DATA"), 0644)

	// Step 2: Start — compactor should NOT be initialized
	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry should not return error, got: %v", err)
	}

	if reg.compactor != nil {
		t.Error("expected compactor to be nil when WAL replay fails")
	}
}

func TestWAL_Lifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "config_wal.log")
	wal := NewWAL(walPath)

	target := &testStruct{Meta: make(map[string]string)}

	// 1. Append
	ops := []PatchOp{
		{Op: OpAdd, Path: "/name", Value: json.RawMessage(`"wal-test"`)},
	}
	commit, err := wal.Append("test-actor", "task-1", ops)
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	if commit.Actor != "test-actor" {
		t.Errorf("expected test-actor, got %s", commit.Actor)
	}

	// 2. Stats
	count, size, err := wal.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 commit, got %d", count)
	}
	if size == 0 {
		t.Error("expected non-zero size")
	}

	// 3. Replay
	if err := wal.Replay(target); err != nil {
		t.Fatalf("Replay failed: %v", err)
	}
	if target.Name != "wal-test" {
		t.Errorf("expected wal-test, got %s", target.Name)
	}

	// 4. Clear
	if err := wal.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	if _, err := os.Stat(walPath); err == nil {
		t.Error("expected wal file to be deleted")
	}
}

func TestRegistry_WALIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"p1"}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	if reg.wal == nil {
		t.Fatal("expected WAL to be initialized")
	}

	// Commit an op
	ops := []PatchOp{
		{Op: OpReplace, Path: "/default_provider", Value: json.RawMessage(`"p2"`)},
	}
	if err := reg.CommitOps(ops, "actor1", "task1"); err != nil {
		t.Fatalf("CommitOps failed: %v", err)
	}

	if reg.DefaultProvider != "p2" {
		t.Errorf("expected p2, got %s", reg.DefaultProvider)
	}

	// Verify WAL file exists and has content
	count, _, _ := reg.wal.Stats()
	if count != 1 {
		t.Errorf("expected 1 commit in WAL, got %d", count)
	}

	// Restart registry and verify replay
	reg2, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry restart failed: %v", err)
	}
	if reg2.DefaultProvider != "p2" {
		t.Errorf("expected p2 after replay, got %s", reg2.DefaultProvider)
	}
}

func TestRegistry_Compaction(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"p1"}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	// 1. Commit multiple ops
	for i := 0; i < 5; i++ {
		ops := []PatchOp{{Op: OpReplace, Path: "/default_provider", Value: json.RawMessage(fmt.Sprintf(`"p%d"`, i+2))}}
		reg.CommitOps(ops, "actor", "task")
	}

	// 2. Verify WAL has 5 entries
	count, _, _ := reg.wal.Stats()
	if count != 5 {
		t.Errorf("expected 5 commits in WAL, got %d", count)
	}

	// 3. Compact
	if err := reg.compactor.Compact(); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	// 4. Verify WAL is cleared
	count, _, _ = reg.wal.Stats()
	if count != 0 {
		t.Errorf("expected 0 commits in WAL after compaction, got %d", count)
	}

	// 5. Verify snapshot on disk is updated
	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), `"p6"`) {
		t.Errorf("snapshot on disk not updated correctly, got: %s", string(data))
	}
}

func TestRegistry_CommitOps_AddRole(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	reg, _ := NewRegistry(configPath)

	role := Role{ID: "new-role", Name: "New Role"}
	roleJSON, _ := json.Marshal(role)
	ops := []PatchOp{
		{Op: OpAdd, Path: "/roles/new-role", Value: json.RawMessage(roleJSON)},
	}

	if err := reg.CommitOps(ops, "admin", "t1"); err != nil {
		t.Fatalf("CommitOps failed: %v", err)
	}

	if reg.Roles["new-role"] == nil || reg.Roles["new-role"].Name != "New Role" {
		t.Errorf("Role not correctly added to memory: %+v", reg.Roles["new-role"])
	}

	// Verify it survives restart
	reg2, _ := NewRegistry(configPath)
	if reg2.Roles["new-role"] == nil || reg2.Roles["new-role"].Name != "New Role" {
		t.Errorf("Role not correctly replayed from WAL: %+v", reg2.Roles["new-role"])
	}
}

func TestRegistry_Rollback(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"p1"}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	reg, _ := NewRegistry(configPath)

	// Commit 1
	ops1 := []PatchOp{{Op: OpReplace, Path: "/default_provider", Value: json.RawMessage(`"p2"`)}}
	reg.CommitOps(ops1, "a1", "t1")

	// Get Commit ID
	f, _ := os.Open(configPath + ".wal")
	var commit Commit
	json.NewDecoder(f).Decode(&commit)
	c1ID := commit.ID
	f.Close()

	// Commit 2
	ops2 := []PatchOp{{Op: OpReplace, Path: "/default_provider", Value: json.RawMessage(`"p3"`)}}
	reg.CommitOps(ops2, "a2", "t2")

	if reg.DefaultProvider != "p3" {
		t.Errorf("expected p3, got %s", reg.DefaultProvider)
	}

	// Rollback to Commit 1
	if err := reg.RollbackTo(c1ID); err != nil {
		t.Fatalf("RollbackTo failed: %v", err)
	}

	if reg.DefaultProvider != "p2" {
		t.Errorf("expected p2 after rollback, got %s", reg.DefaultProvider)
	}

	// Verify WAL is cleared and snapshot is updated
	count, _, _ := reg.wal.Stats()
	if count != 0 {
		t.Errorf("expected WAL cleared after rollback, got %d", count)
	}

	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), `"p2"`) {
		t.Errorf("snapshot not updated correctly after rollback, got: %s", string(data))
	}
}

func TestCompaction_SnapshotRotation(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{"default_provider":"v0"}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	reg, _ := NewRegistry(configPath)
	archiveDir := filepath.Join(tmpDir, "config_archive")

	// Trigger 12 compactions
	for i := 0; i < 12; i++ {
		ops := []PatchOp{{Op: OpReplace, Path: "/default_provider", Value: json.RawMessage(fmt.Sprintf(`"v%d"`, i+1))}}
		reg.CommitOps(ops, "a", "t")
		reg.compactor.Compact()
		// Sleep a bit to ensure unique timestamps in filenames if used (though our prune relies on sort)
		time.Sleep(10 * time.Millisecond)
	}

	// Verify archive dir exists
	if _, err := os.Stat(archiveDir); os.IsNotExist(err) {
		t.Fatal("archive directory should exist")
	}

	// Verify only 10 archives are kept
	files, _ := os.ReadDir(archiveDir)
	count := 0
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), "config_") {
			count++
		}
	}
	if count != 10 {
		t.Errorf("expected 10 archives, got %d", count)
	}
}

// ─── WAL checkpoint / ReplaySince (FIX 2026-09-07) ─────────────────────────
//
// These tests cover the crash-safety gap identified while reviewing this
// file per the 09-06/09-07 playbook addenda's priority list: NewRegistry
// previously called wal.Replay (replay the ENTIRE log) unconditionally on
// every startup, with no way to know that a snapshot on disk might already
// reflect some or all of those commits. If the process crashed between
// persistLocked's snapshot write and the matching wal.Clear() (the two
// non-atomic filesystem operations Compactor.Compact()/PersistForceSnapshot
// perform in sequence), the next startup would replay those
// already-incorporated commits a SECOND time -- which corrupts state for
// any non-idempotent RFC 6902 op (slice "add"/"remove" by index or "-",
// per config/patch.go's setLeaf/applyRemove implementing real INSERT/DELETE
// semantics, not upsert-by-key like map ops).

func TestWAL_LastCommitID_EmptyAndAfterAppends(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")
	wal := NewWAL(walPath)

	if id, err := wal.LastCommitID(); err != nil || id != "" {
		t.Fatalf("expected empty ID and no error for a nonexistent WAL, got id=%q err=%v", id, err)
	}

	c1, err := wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/name", Value: json.RawMessage(`"a"`)}})
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if id, err := wal.LastCommitID(); err != nil || id != c1.ID {
		t.Fatalf("expected LastCommitID=%q after 1 append, got id=%q err=%v", c1.ID, id, err)
	}

	c2, err := wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/name", Value: json.RawMessage(`"b"`)}})
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if id, err := wal.LastCommitID(); err != nil || id != c2.ID {
		t.Fatalf("expected LastCommitID=%q after 2 appends (must be the LAST one, not the first), got id=%q err=%v", c2.ID, id, err)
	}
}

func TestWAL_ReplaySince_SkipsAlreadyAppliedCommits(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")
	wal := NewWAL(walPath)

	c1, err := wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"one"`)}})
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if _, err := wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"two"`)}}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if _, err := wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"three"`)}}); err != nil {
		t.Fatalf("Append 3: %v", err)
	}

	// Simulate: a snapshot was already taken that incorporated commit 1
	// (target already has "one"), then the process crashed before the WAL
	// was cleared. ReplaySince(target, c1.ID) must apply only commits 2 and
	// 3 -- not re-apply commit 1's append.
	target := &testStruct{Items: []string{"one"}}
	if err := wal.ReplaySince(target, c1.ID); err != nil {
		t.Fatalf("ReplaySince: %v", err)
	}

	want := []string{"one", "two", "three"}
	if len(target.Items) != len(want) {
		t.Fatalf("expected %v (commit 1 NOT double-applied), got %v", want, target.Items)
	}
	for i, v := range want {
		if target.Items[i] != v {
			t.Fatalf("expected %v, got %v", want, target.Items)
		}
	}
}

func TestWAL_ReplaySince_EmptyCheckpointReplaysEverything(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")
	wal := NewWAL(walPath)

	_, _ = wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"one"`)}})
	_, _ = wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"two"`)}})

	// Empty checkpoint == "no snapshot has incorporated anything yet" (fresh
	// install, or a snapshot written before this checkpoint mechanism
	// existed) -- must behave exactly like Replay/ReplayUntil("").
	target := &testStruct{}
	if err := wal.ReplaySince(target, ""); err != nil {
		t.Fatalf("ReplaySince: %v", err)
	}
	if len(target.Items) != 2 || target.Items[0] != "one" || target.Items[1] != "two" {
		t.Fatalf("expected both commits applied, got %v", target.Items)
	}
}

func TestWAL_ReplaySince_UnknownCheckpointFailsOpenToFullReplay(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")
	wal := NewWAL(walPath)

	_, _ = wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"one"`)}})
	_, _ = wal.Append("actor", "task", []PatchOp{{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"two"`)}})

	// A checkpoint ID that doesn't exist in this log at all (e.g. the WAL
	// was rotated/truncated after the checkpoint was recorded) must fail
	// OPEN to a full replay -- silently skipping everything because a
	// stale-looking checkpoint wasn't found would be far worse (silent data
	// loss) than the safe-but-redundant alternative of re-applying commits
	// that might already be reflected.
	target := &testStruct{}
	if err := wal.ReplaySince(target, "commit_doesnotexist"); err != nil {
		t.Fatalf("ReplaySince: %v", err)
	}
	if len(target.Items) != 2 || target.Items[0] != "one" || target.Items[1] != "two" {
		t.Fatalf("expected full replay when checkpoint is unknown, got %v", target.Items)
	}
}

func TestRegistry_CrashBetweenSnapshotAndWALClear_DoesNotDoubleApply(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(configPath, []byte(`{}`), 0644)

	os.Setenv("VORTEX_WAL_ENABLED", "true")
	defer os.Unsetenv("VORTEX_WAL_ENABLED")

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Append to a SLICE field (System.Bookmarks) via RFC 6902 "-" (append) --
	// the genuinely non-idempotent case: unlike map keys (upsert-by-key,
	// naturally idempotent), re-applying a slice append inserts a SECOND,
	// duplicate entry.
	ops := []PatchOp{{Op: OpAdd, Path: "/system/bookmarks/-", Value: json.RawMessage(`"example.com"`)}}
	if err := reg.CommitOps(ops, "actor", "task"); err != nil {
		t.Fatalf("CommitOps: %v", err)
	}
	if len(reg.System.Bookmarks) != 1 || reg.System.Bookmarks[0] != "example.com" {
		t.Fatalf("expected 1 bookmark after CommitOps, got %v", reg.System.Bookmarks)
	}

	// Simulate "persistLocked wrote a snapshot that already reflects this
	// commit, but the process crashed before wal.Clear() ran": call the
	// public Persist() (which writes a snapshot but deliberately does NOT
	// clear the WAL -- only PersistForceSnapshot/Compactor.Compact do that)
	// directly, leaving the WAL file with the commit still present on disk.
	if err := reg.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if count, _, _ := reg.wal.Stats(); count != 1 {
		t.Fatalf("expected the commit to still be present in the WAL after a plain Persist() (simulating the crash window), got count=%d", count)
	}

	// Restart: without the fix, NewRegistry would call wal.Replay (the
	// entire log) on top of a snapshot that ALREADY has "example.com" once,
	// producing a duplicate. With the fix, the snapshot's WALCheckpoint
	// tells ReplaySince to skip this already-incorporated commit.
	reg2, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry (restart): %v", err)
	}
	if len(reg2.System.Bookmarks) != 1 || reg2.System.Bookmarks[0] != "example.com" {
		t.Fatalf("expected exactly 1 bookmark after restart (no double-apply), got %v (len=%d)", reg2.System.Bookmarks, len(reg2.System.Bookmarks))
	}
}
