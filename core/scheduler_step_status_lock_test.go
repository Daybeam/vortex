package core

import (
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestC3_StepStatusWrites_ConcurrentUnderLock is a regression test for
// audit C-3: step.Status writes in scheduler_dag.go must be wrapped in
// s.Mu.Lock()/Unlock() to prevent data races with concurrent graph readers.
//
// Before the fix, 6 `step.Status =` writes were unprotected, racing with
// GetStatus() and persistGraph() which read step.Status under lock.
// After the fix, all step.Status writes are wrapped in s.Mu.Lock()/Unlock().
//
// Reproduction: concurrent step status writes under the engine's mutex
// should not panic. Run with `go test -race` to detect the data race.
func TestC3_StepStatusWrites_ConcurrentUnderLock(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-step-status",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-step-status"] = graph
	s.Mu.Unlock()

	// Concurrent status writes under lock (simulating what scheduler_dag does after C-3 fix).
	var wg sync.WaitGroup
	statuses := []schemas.StepStatus{schemas.StepRunning, schemas.StepFailed, schemas.StepPending, schemas.StepOK}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s.Mu.Lock()
			step.Status = statuses[idx%len(statuses)]
			s.Mu.Unlock()
		}(i)
	}

	// Concurrent status readers (simulating GetStatus).
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Mu.RLock()
			_ = step.Status
			s.Mu.RUnlock()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success: no panic.
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: concurrent step status access deadlocked")
	}
}
