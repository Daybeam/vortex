package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// audit M3 (2026-11-05): regression test for TaskStore without Mu lock.
// The lock was removed because the backend (FileTaskBackend / SQLiteTaskBackend)
// is thread-safe. This test verifies concurrent Set/Get/ClearTask operations
// work correctly without the TaskStore-level lock.

func TestTaskStore_ConcurrentSetGet(t *testing.T) {
	backend := NewFileTaskBackend(t.TempDir())
	ts := NewTaskStore(backend)
	ctx := context.Background()

	const N = 50
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			result := &StepResult{
				Data:       fmt.Sprintf("result-%d", n),
				Confidence: 0.9,
			}
			if err := ts.Set(ctx, "task_concurrent", fmt.Sprintf("step-%d", n), result); err != nil {
				t.Errorf("Set failed: %v", err)
			}
		}(i)
	}

	// Concurrent readers (may read before writes complete — that's fine,
	// we just verify no panics or corruption).
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = ts.Get(ctx, "task_concurrent", fmt.Sprintf("step-%d", n))
		}(i)
	}

	wg.Wait()

	// Verify all written entries are readable.
	for i := 0; i < N; i++ {
		stepID := fmt.Sprintf("step-%d", i)
		res, err := ts.Get(ctx, "task_concurrent", stepID)
		if err != nil {
			t.Errorf("Get(%s) failed: %v", stepID, err)
			continue
		}
		want := fmt.Sprintf("result-%d", i)
		if res.Data != want {
			t.Errorf("Get(%s) = %v, want %q", stepID, res.Data, want)
		}
	}
}

func TestTaskStore_ConcurrentClearTask(t *testing.T) {
	backend := NewFileTaskBackend(t.TempDir())
	ts := NewTaskStore(backend)
	ctx := context.Background()

	// Seed data.
	for i := 0; i < 10; i++ {
		ts.Set(ctx, "task_clear", fmt.Sprintf("step-%d", i), &StepResult{Data: "data"})
	}

	// Concurrent clear + get.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ts.ClearTask(ctx, "task_clear")
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			ts.Get(ctx, "task_clear", fmt.Sprintf("step-%d", i))
		}
	}()
	wg.Wait()
}
