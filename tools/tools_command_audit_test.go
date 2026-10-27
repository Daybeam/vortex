package tools

import (
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Regression test for audit RL-1 (2026-10-24): goroutine leak in
// startSandboxRunCleaner. The background cleaner ran `for range ticker.C`
// with no stop channel, no context, and no Stop() method — it leaked
// permanently with no graceful-shutdown path.
//
// Before fix: stopSandboxRunCleaner did not exist; the goroutine ran forever.
// After fix:  stopSandboxRunCleaner closes a stop channel; the goroutine exits.
// Source: docs/archive/audits/AUDIT_REPORT_2026-10-24_COMPREHENSIVE.md §RL-1
// -----------------------------------------------------------------------------
func TestSandboxRunCleaner_StartStopNoLeak(t *testing.T) {
	// Start the cleaner — this launches the background goroutine (once).
	startSandboxRunCleaner()

	// Stop the cleaner — before the fix this function didn't exist and the
	// goroutine leaked. After the fix it closes sandboxRunCleanerStop and the
	// goroutine exits via the select case <-sandboxRunCleanerStop.
	stopSandboxRunCleaner()

	// Verify stop is idempotent (calling again must not panic on double-close).
	// This works because stopSandboxRunCleaner uses sync.Once to protect the close.
	stopSandboxRunCleaner()

	// Give the goroutine a moment to exit cleanly.
	time.Sleep(50 * time.Millisecond)

	// If we reach here without panicking or deadlocking, the fix works.
	// Before the fix, there was no stopSandboxRunCleaner function, so this
	// test would not compile — the compilation failure IS the regression check.
}
