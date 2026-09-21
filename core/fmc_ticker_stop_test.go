package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// Regression test for H3: sync.Once in FMCBatchTicker.Stop().
//
// Before the fix, Stop() called close(t.stopCh) unconditionally, so a second
// Stop() would panic on double-close. The fix wrapped the body in sync.Once.
//
// The existing fmc_ticker_test.go already has TestFMCBatchTicker_Start_NoOpWhen-
// WeakModelUnconfigured which calls Stop() once. This file adds the repeated-
// Stop safety test that the H3 fix specifically addresses.

// TestFMCBatchTicker_Stop_Idempotent verifies that calling Stop() multiple
// times does not panic (double-close safety via sync.Once).
func TestFMCBatchTicker_Stop_Idempotent(t *testing.T) {
	// Construct via NewFMCBatchTicker to get a properly initialized stopOnce.
	reg := &config.Registry{}
	es := newFMCTestExperienceStore(t)
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	defer logger.Close()

	ticker := NewFMCBatchTicker(reg, config.ExternalRuntimes{}, es, logger)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked on repeated call: %v", r)
		}
	}()
	ticker.Stop()
	ticker.Stop() // must not panic
	ticker.Stop() // third call for good measure
}

// TestFMCBatchTicker_Stop_IdempotentBareLiteral verifies the same safety on
// a bare struct literal (no NewFMCBatchTicker), which is the scenario most
// likely to trip a missing sync.Once: a zero-value stopOnce is still valid
// (sync.Once zero value is ready to use).
func TestFMCBatchTicker_Stop_IdempotentBareLiteral(t *testing.T) {
	ticker := &FMCBatchTicker{
		stopCh: make(chan struct{}),
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked on repeated call with bare literal: %v", r)
		}
	}()
	ticker.Stop()
	ticker.Stop()
}

// Ensure newFMCTestExperienceStore is available — it's defined in
// fmc_ticker_test.go in the same package. This is a compile-time reference
// to catch accidental removal.
var _ = newFMCTestExperienceStore

// Ensure store package is referenced (used by newFMCTestExperienceStore).
var _ = store.ExperienceNode{}
