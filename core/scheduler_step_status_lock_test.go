package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// stubTaskStore is a minimal ITaskStore for testing ClaimStep without a real DB.
// regression for audit TEST-1: the old test reimplemented locking in the test
// goroutines instead of calling the real production code.
type stubTaskStore struct{}

func (s *stubTaskStore) Set(ctx context.Context, taskID, stepID string, result *store.StepResult) error {
	return nil
}
func (s *stubTaskStore) Get(ctx context.Context, taskID, stepID string) (*store.StepResult, error) {
	return nil, nil
}
func (s *stubTaskStore) GetBatch(ctx context.Context, taskID string, stepIDs []string) (map[string]*store.StepResult, error) {
	return nil, nil
}
func (s *stubTaskStore) GetByRef(ctx context.Context, ref string) (*store.StepResult, error) {
	return nil, nil
}
func (s *stubTaskStore) ClearTask(ctx context.Context, taskID string) (int, error) {
	return 0, nil
}
func (s *stubTaskStore) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}

// TestC3_StepStatusWrites_ConcurrentUnderLock is a regression test for
// audit C-3: step.Status writes in scheduler_dag.go must be wrapped in
// s.Mu.Lock()/Unlock() to prevent data races with concurrent graph readers.
//
// This test calls the REAL production methods (ClaimStep + GetStatus) instead
// of reimplementing the locking in the test. If someone removes s.Mu.Lock()
// from ClaimStep, `go test -race` will detect the data race.
// regression for audit TEST-1: old version was tautological (tested the lock, not the code).
func TestC3_StepStatusWrites_ConcurrentUnderLock(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()
	s.taskStore = &stubTaskStore{} // wire minimal stub so ClaimStep doesn't panic on nil

	graph := &schemas.TaskGraph{
		TaskID: "task-step-status",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepPending}

	s.Mu.Lock()
	s.graphs["task-step-status"] = graph
	s.Mu.Unlock()

	var wg sync.WaitGroup

	// Concurrent REAL ClaimStep calls (writes step.Status under s.Mu.Lock in production).
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Reset to pending so ClaimStep can claim it again
			s.Mu.Lock()
			if step := s.graphs["task-step-status"].Steps["s1"]; step != nil {
				step.Status = schemas.StepPending
			}
			s.Mu.Unlock()
			s.ClaimStep("task-step-status", "s1") // REAL production method
		}()
	}

	// Concurrent REAL GetStatus calls (reads step.Status under s.Mu.RLock in production).
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.GetStatus("task-step-status") // REAL production method
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success: no panic, no race.
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: concurrent step status access deadlocked")
	}
}
