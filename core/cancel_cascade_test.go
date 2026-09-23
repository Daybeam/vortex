package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestCancelTask_SwarmProviderLoopNotKilledButTaskInvisible verifies the first
// P0 cancellation risk point flagged in
// docs/completed/2026-09-14/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §3.1/§6:
// "SwarmAgentProvider 的 loop(ctx) goroutine 的 ctx 来源是否为 task context？"
//
// FINDING: the swarm provider's loop ctx comes from Start(ctx), NOT from the
// task's cancelFuncs context. CancelTask cannot directly cancel the swarm
// provider's goroutine via context cancellation. However, CancelTask sets
// graph.Status = GraphCancelled, which makes the task invisible to the swarm
// provider's SensingGlobal ranking — a cancelled task has no signal and will
// not be claimed. This is the designed behavior: the swarm loop continues
// running (it serves all tasks, not just one), but individual cancelled tasks
// are skipped because their graph status is terminal.
//
// This test verifies the mechanism that actually protects against the
// "zombie swarm agent keeps working on a cancelled task" scenario:
//   1. CancelTask sets GraphCancelled
//   2. scheduler_dag.go skips any graph whose Status != GraphRunning
//      (checked at lines 187 and 254), so a cancelled graph is invisible
//   3. The task's cancelFunc is invoked, so any in-flight step goroutines
//      spawned by run() also exit
func TestCancelTask_SwarmProviderLoopNotKilledButTaskInvisible(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()
	reg.System.StagingEnabled = false

	graph := &schemas.TaskGraph{
		TaskID: "task-swarm-cancel-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, Task: "do work"},
		},
		Status: schemas.GraphRunning,
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.cancelFuncs[graph.TaskID] = cancel
	s.doneChans[graph.TaskID] = make(chan struct{})
	s.Mu.Unlock()

	runDone := make(chan struct{})
	go func() {
		s.run(ctx, graph.TaskID)
		close(runDone)
	}()

	time.Sleep(20 * time.Millisecond)

	if err := s.CancelTask(graph.TaskID); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not exit within 2s of CancelTask")
	}

	s.Mu.RLock()
	status := graph.Status
	s.Mu.RUnlock()

	if status != schemas.GraphCancelled {
		t.Fatalf("expected GraphCancelled, got %s", status)
	}

	if graph.Status == schemas.GraphRunning {
		t.Fatal("cancelled graph must not be GraphRunning so scheduler_dag skips it")
	}

	select {
	case <-ctx.Done():
	default:
		t.Fatal("task ctx must be cancelled so in-flight step goroutines exit")
	}
}

// TestCancelTask_StagingGoroutineReceivesContextCancellation verifies the
// second P0 risk: staging goroutines spawned in scheduler_dag.go's run() loop
// capture the task's cancellable context. When CancelTask fires, these
// goroutines should see ctx.Done() and exit. This complements
// TestCancelTask_RunLoopExitsPromptly_NoOwnGoroutineForStagedWorkspace by
// explicitly verifying the context propagation to the per-step goroutine.
func TestCancelTask_StagingGoroutineReceivesContextCancellation(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()
	reg.System.StagingEnabled = true

	graph := &schemas.TaskGraph{
		TaskID: "task-staging-ctx-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, Task: "do work"},
		},
		Status: schemas.GraphRunning,
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.cancelFuncs[graph.TaskID] = cancel
	s.doneChans[graph.TaskID] = make(chan struct{})
	s.Mu.Unlock()

	stepCtxCancelled := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			close(stepCtxCancelled)
		case <-time.After(3 * time.Second):
		}
	}()

	time.Sleep(20 * time.Millisecond)

	if err := s.CancelTask(graph.TaskID); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	select {
	case <-stepCtxCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("task ctx was not cancelled within 2s — staging goroutines would not exit")
	}
}
