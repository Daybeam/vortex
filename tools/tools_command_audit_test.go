package tools

import (
	"runtime"
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
	// Count goroutines before starting the cleaner.
	before := runtime.NumGoroutine()

	// Start the cleaner — this launches the background goroutine (once).
	startSandboxRunCleaner()

	// Stop the cleaner — before the fix this function didn't exist and the
	// goroutine leaked. After the fix it closes sandboxRunCleanerStop and the
	// goroutine exits via the select case <-sandboxRunCleanerStop.
	stopSandboxRunCleaner()

	// Verify stop is idempotent (calling again must not panic on double-close).
	// This works because stopSandboxRunCleaner uses sync.Once to protect the close.
	stopSandboxRunCleaner()

	// Poll for goroutine exit with a timeout instead of a fixed Sleep.
	// The goroutine should exit within a few milliseconds, but we give it
	// up to 500ms to account for scheduler latency.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine leak: before=%d, after=%d (cleaner goroutine did not exit)", before, after)
	}
}
