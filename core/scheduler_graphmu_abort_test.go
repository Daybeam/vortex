package core

import (
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestC2_GraphMu_SetAndProtectsDecisionHistory is a regression test for
// audit C-2 (updated for P-2.1 Step 2): DecisionHistory is now protected by
// TaskGraph.decisionMu via RecordDecision/UpdateDecisionOutcome methods,
// decoupled from DirectedEngine.Mu.
//
// Before the original fix (C-2), ContextHub had no mutex for graph access.
// Before P-2.1 Step 2, GraphMu pointed to &s.Mu, blocking all graph ops
// during DecisionHistory writes. Now TaskGraph has its own decisionMu.
//
// Reproduction: verify concurrent DecisionHistory writes via RecordDecision
// do not panic.
func TestC2_GraphMu_SetAndProtectsDecisionHistory(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID:      "task-graphmu-test",
		Steps:       make(map[string]*schemas.Step),
		Status:      schemas.GraphRunning,
		ContextTree: make(map[string]*schemas.ContextNode),
	}
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.ContextTree["root"] = &schemas.ContextNode{ID: "root"}

	s.Mu.Lock()
	s.graphs["task-graphmu-test"] = graph
	s.Mu.Unlock()

	// Concurrent DecisionHistory writes via RecordDecision should not panic.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			graph.RecordDecision("decision-"+string(rune('A'+idx%26)), &schemas.DecisionNode{})
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: concurrent DecisionHistory writes deadlocked")
	}

	if len(graph.DecisionHistory) == 0 {
		t.Errorf("expected non-zero decisions, got %d", len(graph.DecisionHistory))
	}
}

// TestC4_RequestAutonomousAbort_ConcurrentNoPanic is a regression test for
// audit C-4: RequestAutonomousAbort must read the graphs map inside RLock
// to avoid racing with concurrent graph creation/deletion.
//
// Before the fix, the map read was outside the lock, causing a data race
// when concurrent goroutines called RequestAutonomousAbort.
// After the fix, the map read is inside RLock.
//
// Reproduction: call RequestAutonomousAbort concurrently and verify no panic.
func TestC4_RequestAutonomousAbort_ConcurrentNoPanic(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-concurrent-abort",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepRunning}

	s.Mu.Lock()
	s.graphs["task-concurrent-abort"] = graph
	s.Mu.Unlock()

	var wg sync.WaitGroup
	const numGoroutines = 50

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RequestAutonomousAbort("task-concurrent-abort", "s1", FailureClassCostOverrun, "test")
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
		t.Fatal("timeout: concurrent RequestAutonomousAbort deadlocked")
	}
}
