package config

import (
	"testing"
	"time"
)

// TestNewRegistry_MissingConfigFile_DoesNotDeadlock guards against a regression
// of the 2026-07-10 fix: Load() holds r.Mu.Lock() for its entire body, and its
// os.ReadFile error branch used to call bootstrapDefaults() (which itself takes
// r.Mu.Lock() again). Since sync.Mutex is not re-entrant, this deadlocked any
// caller of NewRegistry/Load whenever the config file was missing or briefly
// unreadable (e.g. during an external hot-edit window). Discovered via
// TestAgnesProvider hanging indefinitely in the providers package -- that test
// pointed at a config.json path that doesn't exist relative to the providers/
// package's test working directory, which reliably triggered this branch.
//
// This test must complete well within the timeout; if the deadlock regresses,
// this test will hang until the outer `go test -timeout` kills the whole run
// with a goroutine dump rather than failing cleanly, so keep the internal
// select timeout comfortably shorter than any reasonable `go test -timeout`.
func TestNewRegistry_MissingConfigFile_DoesNotDeadlock(t *testing.T) {
	done := make(chan struct{})
	var reg *Registry
	var err error

	go func() {
		reg, err = NewRegistry("this_config_file_does_not_exist_12345.json")
		close(done)
	}()

	select {
	case <-done:
		// Good: returned promptly instead of deadlocking.
	case <-time.After(5 * time.Second):
		t.Fatal("NewRegistry deadlocked on a missing config file (bootstrapDefaults double-lock regression)")
	}

	if err != nil {
		t.Fatalf("expected NewRegistry to bootstrap defaults on missing file, got error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected a non-nil Registry with bootstrapped defaults")
	}
	if _, ok := reg.Providers["default"]; !ok {
		t.Error("expected bootstrapDefaultsLocked to have populated a 'default' provider")
	}

	// A second Load() call (simulating StartWatcher's periodic reload hitting
	// the same missing-file path again) must also not deadlock.
	done2 := make(chan struct{})
	go func() {
		_ = reg.Load()
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("second Load() call deadlocked on a missing config file")
	}
}
