package core

import (
	"sync"
	"testing"
	"time"
)

// Regression tests for M7: JIT session manager lock released during spawn.
//
// Before the fix, GetOrCreateSession held m.mu.Lock() during the entire
// subprocess spawn (which can take hundreds of milliseconds for Python
// startup). This serialized all session lookups/creations under concurrent
// load. The fix uses double-check locking: lock → check cache → unlock →
// spawn → lock → double-check → store → unlock, so the lock is only held
// for the brief map operations, not during the slow spawn.
//
// These tests verify that concurrent GetOrCreateSession calls complete
// without deadlock. They require Python on PATH (same as the existing
// jit_session_test.go suite).

// TestJITSessionManager_ConcurrentGetOrCreateSession_NoDeadlock launches
// multiple goroutines that each create a session with a unique ID, then
// verifies all complete within a generous timeout. If the lock were held
// during spawn in a way that causes deadlock, this test would time out.
func TestJITSessionManager_ConcurrentGetOrCreateSession_NoDeadlock(t *testing.T) {
	m := newTestJITSessionManager(t)

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)

	start := time.Now()
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			sess, err := m.GetOrCreateSession(
				"concurrent-test-"+string(rune('A'+idx)),
				"python",
				30*time.Second,
			)
			if err != nil {
				errs[idx] = err
				return
			}
			m.CloseSession(sess.ID)
		}(i)
	}

	// Wait with a timeout. 5 concurrent Python spawns should complete
	// well under 30s even on a slow machine. If this times out, there's
	// likely a deadlock in GetOrCreateSession's locking.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// all goroutines completed
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent GetOrCreateSession calls deadlocked — timed out after 30s")
	}

	elapsed := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
	}
	t.Logf("%d concurrent sessions created in %v", n, elapsed)
}

// TestJITSessionManager_ConcurrentSameSessionID_ReturnsSameSession verifies
// that concurrent calls with the SAME session ID all get the same session
// (the double-check locking pattern must not create duplicate sessions).
func TestJITSessionManager_ConcurrentSameSessionID_ReturnsSameSession(t *testing.T) {
	m := newTestJITSessionManager(t)
	defer m.CloseSession("concurrent-same-id")

	const n = 4
	var wg sync.WaitGroup
	wg.Add(n)
	sessions := make([]*JITSession, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			sessions[idx], errs[idx] = m.GetOrCreateSession(
				"concurrent-same-id",
				"python",
				30*time.Second,
			)
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent same-ID GetOrCreateSession calls timed out — potential deadlock")
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d error: %v", i, err)
		}
	}

	// All goroutines must have received the same *JITSession pointer.
	first := sessions[0]
	for i := 1; i < n; i++ {
		if sessions[i] != first {
			t.Errorf("goroutine %d got a different session pointer — double-check locking created duplicate sessions", i)
		}
	}
	if got := m.SessionCount(); got != 1 {
		t.Errorf("expected exactly 1 session after concurrent same-ID creation, got %d", got)
	}
}
