package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// TestR3_JITSessionManager_CloseAllStopsTTLGoroutines is a regression test
// for audit R-3: JITSessionManager.CloseAll must close stopCh so that all
// TTL goroutines exit promptly instead of lingering until their timer fires.
//
// Before the fix, there was no stopCh. TTL goroutines waited on timer.C
// alone, so they lingered for up to ttl duration after CloseAll was called.
// After the fix, CloseAll closes stopCh, causing TTL goroutines to exit
// via the `case <-m.stopCh` branch.
//
// Reproduction: after CloseAll(), stopCh must be closed (receive succeeds).
func TestR3_JITSessionManager_CloseAllStopsTTLGoroutines(t *testing.T) {
	reg := &config.Registry{}
	m := NewJITSessionManager(reg, t.TempDir())

	// stopCh should be open (not closed) initially.
	select {
	case <-m.stopCh:
		t.Fatal("stopCh should be open before CloseAll()")
	default:
		// Expected: stopCh is open.
	}

	// Call CloseAll — should close stopCh.
	m.CloseAll()

	// stopCh should now be closed (receive succeeds immediately).
	select {
	case <-m.stopCh:
		// Success: stopCh is closed.
	default:
		t.Fatal("stopCh should be closed after CloseAll()")
	}

	// CloseAll should be idempotent (stopOnce protects the close).
	m.CloseAll() // should not panic
}

// TestR3_JITSessionManager_TTLGoroutineExitsOnCloseAll verifies that a TTL
// goroutine exits promptly when CloseAll is called, rather than waiting for
// its timer to fire.
func TestR3_JITSessionManager_TTLGoroutineExitsOnCloseAll(t *testing.T) {
	reg := &config.Registry{}
	m := NewJITSessionManager(reg, t.TempDir())

	// We can't easily create a real session (requires Python), but we can
	// verify the TTL goroutine pattern: a goroutine waiting on timer.C or
	// stopCh should exit when stopCh is closed.
	exited := make(chan struct{})

	go func() {
		timer := time.NewTimer(10 * time.Second) // long TTL
		defer timer.Stop()
		select {
		case <-timer.C:
			// Would only happen if R-3 fix is broken.
		case <-m.stopCh:
			close(exited) // R-3 fix: exits promptly
		}
	}()

	// CloseAll should cause the goroutine to exit immediately.
	m.CloseAll()

	select {
	case <-exited:
		// Success: TTL goroutine exited promptly.
	case <-time.After(time.Second):
		t.Fatal("timeout: TTL goroutine did not exit after CloseAll() — R-3 fix not working")
	}
}
