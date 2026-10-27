package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// -----------------------------------------------------------------------------
// Regression test for audit PF-1 (2026-10-24): reentrant RLock deadlock in
// CapabilityNavigator.ClassifyIntent. ExtractCapabilities/ClassifyIntent
// acquired Registry.Mu.RLock() then called ResolveProviderConfig which
// re-acquired the same RLock. Go's sync.RWMutex is NOT reentrant — if a
// writer is waiting between the two RLock calls, the second RLock blocks
// waiting for the writer, while the writer waits for the first RLock to
// release → deadlock.
//
// Before fix: this test deadlocks (times out).
// After fix:  the lock is released before ResolveProviderConfig is called.
// Source: docs/archive/audits/AUDIT_REPORT_2026-10-24_COMPREHENSIVE.md §PF-1
// -----------------------------------------------------------------------------
func TestClassifyIntent_NoReentrantRLockDeadlock(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"r1": {ID: "r1", Name: "Role1", Provider: "default"},
		},
		Providers: map[string]*config.ProviderConfig{
			"default": {Provider: "stub", Model: "stub-model", APIKeyEnv: "STUB_KEY"},
		},
		MCPs:            make(map[string]*config.MCPDef),
		DefaultProvider: "default",
	}
	nav := NewCapabilityNavigator(reg)

	// Spawn a writer that repeatedly acquires the registry write lock,
	// creating the contention that triggers the reentrant-RLock deadlock.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			reg.Mu.Lock()
			// Brief hold to create contention window.
			reg.Mu.Unlock()
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	// Call ClassifyIntent concurrently — before the fix this deadlocks
	// because the reader holds RLock, the writer waits for Lock, and the
	// reader's inner ResolveProviderConfig blocks on RLock (writer waiting).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultCh := make(chan []string, 1)
	go func() {
		resultCh <- nav.ClassifyIntent(ctx, "search for papers and compare results")
	}()

	select {
	case <-resultCh:
		// Success — no deadlock.
	case <-ctx.Done():
		t.Fatal("ClassifyIntent deadlocked (reentrant RLock) — see audit PF-1")
	}

	close(done)
	wg.Wait()
}
