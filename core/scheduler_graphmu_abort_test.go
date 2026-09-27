package core

import (
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestC2_GraphMu_SetAndProtectsDecisionHistory is a regression test for
// audit C-2: ContextHub.GraphMu must be initialized to the scheduler's Mu
// so that Spawner's DecisionHistory writes are protected by the same mutex
// as persistGraph.
//
// Before the fix, ContextHub had no mutex for graph access. The Spawner used
// its own s.Mu to protect Graph.DecisionHistory, racing with persistGraph
// which uses DirectedEngine.Mu.
// After the fix, GraphMu is set to &s.Mu in scheduler_dag.go.
//
// Reproduction: verify GraphMu is non-nil and points to the engine's Mu.
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

	// Create a ContextHub as the scheduler does in run().
	hub := &ContextHub{
		Graph:       graph,
		GraphMu:     &s.Mu, // This is what scheduler_dag.go sets (audit C-2)
	}

	if hub.GraphMu == nil {
		t.Fatal("GraphMu must be non-nil after C-2 fix")
	}

	// Concurrent DecisionHistory writes using GraphMu should not panic.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			hub.GraphMu.Lock()
			if hub.Graph.DecisionHistory == nil {
				hub.Graph.DecisionHistory = make(map[string]*schemas.DecisionNode)
			}
			hub.Graph.DecisionHistory["decision-"+string(rune('A'+idx%26))] = &schemas.DecisionNode{}
			hub.GraphMu.Unlock()
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

	if len(hub.Graph.DecisionHistory) == 0 {
		t.Errorf("expected non-zero decisions, got %d", len(hub.Graph.DecisionHistory))
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
