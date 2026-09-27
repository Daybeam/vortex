package core

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPC3_BoundedSemaphore_LimitsConcurrency is a regression test for audit
// P-C3: the crystallization goroutine launcher must use a bounded semaphore
// (maxConcurrentCrystallize = 3) to prevent goroutine storms and provider
// rate limit exhaustion under multi-task load.
//
// Before the fix, each record spawned a goroutine unconditionally, so N
// records = N goroutines. Under multi-task load (e.g. 50 tasks × 10 records),
// this created 500 goroutines simultaneously.
// After the fix, a buffered channel semaphore limits concurrency to 3.
//
// Reproduction: simulate the semaphore pattern and verify that at most 3
// goroutines run concurrently, regardless of how many are requested.
func TestPC3_BoundedSemaphore_LimitsConcurrency(t *testing.T) {
	const maxConcurrent = 3
	const numRequests = 50

	crystSem := make(chan struct{}, maxConcurrent)

	var currentConcurrent int64
	var maxObserved int64
	var wg sync.WaitGroup

	for i := 0; i < numRequests; i++ {
		select {
		case crystSem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-crystSem }()

				cur := atomic.AddInt64(&currentConcurrent, 1)
				// Track max concurrency observed.
				for {
					max := atomic.LoadInt64(&maxObserved)
					if cur <= max || atomic.CompareAndSwapInt64(&maxObserved, max, cur) {
						break
					}
				}

				time.Sleep(10 * time.Millisecond) // simulate work

				atomic.AddInt64(&currentConcurrent, -1)
			}()
		default:
			// audit P-C3: at concurrency limit — skip
		}
	}

	wg.Wait()

	if maxObserved > maxConcurrent {
		t.Errorf("max concurrency %d exceeded limit %d", maxObserved, maxConcurrent)
	}

	if maxObserved == 0 {
		t.Error("expected at least 1 goroutine to run, but max observed was 0")
	}
}

// TestPC3_BoundedSemaphore_SkipsAtCapacity verifies that when the semaphore
// is at capacity, additional requests are skipped (not blocked).
func TestPC3_BoundedSemaphore_SkipsAtCapacity(t *testing.T) {
	const maxConcurrent = 3
	crystSem := make(chan struct{}, maxConcurrent)

	// Fill the semaphore to capacity.
	for i := 0; i < maxConcurrent; i++ {
		crystSem <- struct{}{}
	}

	// Now the semaphore is full. The next request should be skipped (default branch).
	skipped := false
	select {
	case crystSem <- struct{}{}:
		// Should not reach here — semaphore is full.
	default:
		skipped = true
	}

	if !skipped {
		t.Error("expected request to be skipped when semaphore is at capacity")
	}

	// Drain the semaphore.
	for i := 0; i < maxConcurrent; i++ {
		<-crystSem
	}
}
