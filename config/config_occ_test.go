package config

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCheckSplitLayoutVersion_NilAndSupportedPass(t *testing.T) {
	if err := checkSplitLayoutVersion(nil); err != nil {
		t.Fatalf("nil layout should always pass, got %v", err)
	}
	if err := checkSplitLayoutVersion(&SplitLayout{Version: currentSplitLayoutVersion}); err != nil {
		t.Fatalf("current version should pass, got %v", err)
	}
	if err := checkSplitLayoutVersion(&SplitLayout{Version: 1}); err != nil {
		t.Fatalf("older version should pass, got %v", err)
	}
}

func TestCheckSplitLayoutVersion_TooNewFails(t *testing.T) {
	err := checkSplitLayoutVersion(&SplitLayout{Version: currentSplitLayoutVersion + 1})
	if err == nil {
		t.Fatal("expected an error for an unsupported (too new) split layout version")
	}
	if !errors.Is(err, ErrUnsupportedSplitLayoutVersion) {
		t.Fatalf("expected error to wrap ErrUnsupportedSplitLayoutVersion, got: %v", err)
	}
}

// TestLoad_RefusesUnsupportedSplitVersion_PreservesPreviousState is a direct
// regression test for the 2026-07-23 production incident: an older binary
// silently emptied its in-memory MCPs/Roles after loading a config whose
// SplitLayout declared a version it didn't understand. This test asserts
// the fixed behavior: Load() returns an error and leaves the previously-
// loaded in-memory state completely untouched.
func TestLoad_RefusesUnsupportedSplitVersion_PreservesPreviousState(t *testing.T) {
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 3)
	if err := r.Persist(); err != nil {
		t.Fatalf("initial Persist: %v", err)
	}

	// Reload once normally to establish a known-good in-memory baseline
	// (this also exercises the normal, supported-version path).
	if err := r.Load(); err != nil {
		t.Fatalf("baseline Load: %v", err)
	}
	if len(r.MCPs) != 3 {
		t.Fatalf("expected 3 MCPs after baseline load, got %d", len(r.MCPs))
	}

	// Now simulate an external process writing a config that declares a
	// split layout version newer than this binary supports.
	data, err := os.ReadFile(r.GetConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.SplitLayout = &SplitLayout{Version: currentSplitLayoutVersion + 1, MCPs: "workspace/mcps/"}
	newData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(r.GetConfigPath(), newData, 0644); err != nil {
		t.Fatalf("write future-versioned config: %v", err)
	}

	err = r.Load()
	if err == nil {
		t.Fatal("expected Load() to refuse a config with an unsupported split layout version")
	}
	if !errors.Is(err, ErrUnsupportedSplitLayoutVersion) {
		t.Fatalf("expected error to wrap ErrUnsupportedSplitLayoutVersion, got: %v", err)
	}

	// The critical assertion: previous in-memory state must be untouched,
	// not silently emptied.
	if len(r.MCPs) != 3 {
		t.Fatalf("expected previous in-memory MCPs to be preserved after a refused reload, got %d", len(r.MCPs))
	}
}

func TestPersist_OCC_FreshRegistryAllowsFirstWrite(t *testing.T) {
	r := newSplitTestRegistry(t)
	// Registry was just created via NewRegistry against a nonexistent file
	// (bootstrapDefaultsLocked path) -- loadedModTime should be zero, and
	// the very first Persist() must be allowed to proceed.
	if err := r.Persist(); err != nil {
		t.Fatalf("expected first Persist() on a fresh registry to succeed, got: %v", err)
	}
}

func TestPersist_OCC_AllowsWhenFileUnchangedSinceLoad(t *testing.T) {
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 1)
	if err := r.Persist(); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Nothing external touched the file since Load() -- this Persist()
	// should succeed normally.
	addTestMCPs(r, 1)
	if err := r.Persist(); err != nil {
		t.Fatalf("expected Persist() to succeed when file is unchanged since load, got: %v", err)
	}
}

func TestPersist_OCC_RejectsWhenFileChangedSinceLoad(t *testing.T) {
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 1)
	if err := r.Persist(); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Simulate an external process (a human hand-edit, or a concurrent
	// agent) modifying the file after our Load() but before our next
	// Persist() -- the exact race documented in the 2026-06-29 addendum.
	simulateExternalEdit(t, r.GetConfigPath())

	err := r.Persist()
	if err == nil {
		t.Fatal("expected Persist() to refuse writing over an externally-modified file")
	}
	if !errors.Is(err, ErrConfigChangedSinceLoad) {
		t.Fatalf("expected error to wrap ErrConfigChangedSinceLoad, got: %v", err)
	}

	// And confirm the external edit survived -- Persist() must not have
	// touched the file at all when it refused.
	data, readErr := os.ReadFile(r.GetConfigPath())
	if readErr != nil {
		t.Fatalf("read config after refused persist: %v", readErr)
	}
	if !containsSentinel(data) {
		t.Fatal("expected the externally-written sentinel content to survive a refused Persist()")
	}
}

func TestPersist_OCC_EnvDisableBypasses(t *testing.T) {
	t.Setenv("VORTEX_CONFIG_NO_OCC", "1")
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 1)
	if err := r.Persist(); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	simulateExternalEdit(t, r.GetConfigPath())

	if err := r.Persist(); err != nil {
		t.Fatalf("expected Persist() to succeed with VORTEX_CONFIG_NO_OCC=1 despite external edit, got: %v", err)
	}
}

// TestPersist_OCC_ConsecutiveWritesDoNotSelfConflict guards against a
// regression where persistAndSnapshot fails to update the loaded snapshot
// after its own successful write, which would make every second Persist()
// in the same process incorrectly look like an external conflict.
func TestPersist_OCC_ConsecutiveWritesDoNotSelfConflict(t *testing.T) {
	r := newSplitTestRegistry(t)
	addTestMCPs(r, 1)
	if err := r.Persist(); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 0; i < 3; i++ {
		addTestMCPs(r, 1)
		if err := r.Persist(); err != nil {
			t.Fatalf("consecutive Persist() #%d unexpectedly rejected as stale: %v", i, err)
		}
	}
}

// -- small helpers local to this test file --

func simulateExternalEdit(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read for external edit simulation: %v", err)
	}
	// Ensure the mtime genuinely advances even on filesystems with coarse
	// mtime resolution, and append a byte so the size also changes -- either
	// signal alone should trip checkNotStaleForPersist, but both together
	// makes this test robust to platform timer granularity.
	data = append(data, ' ')
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write external edit: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func containsSentinel(data []byte) bool {
	// simulateExternalEdit appends a trailing space byte; if the file was
	// overwritten by a would-be Persist(), that trailing byte (and the
	// original JSON's exact byte length) would not match.
	return len(data) > 0 && data[len(data)-1] == ' '
}
