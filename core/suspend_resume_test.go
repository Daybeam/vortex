package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestSuspendTask_RunningTask_GetsSuspended verifies that suspending a running
// task transitions its status to GraphSuspended and persists the state.
func TestSuspendTask_RunningTask_GetsSuspended(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-suspend-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, Task: "do work"},
		},
		Status: schemas.GraphRunning,
	}

	injectRunningTestGraph(t, s, graph)

	if err := s.SuspendTask("task-suspend-1"); err != nil {
		t.Fatalf("SuspendTask failed: %v", err)
	}

	g := s.GetTaskGraph("task-suspend-1")

	if g.Status != schemas.GraphSuspended {
		t.Fatalf("expected status %q, got %q", schemas.GraphSuspended, g.Status)
	}
}

func TestSuspendTask_NonRunningTask_ReturnsError(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-suspend-2",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphCompleted,
	}

	injectRunningTestGraph(t, s, graph)

	err := s.SuspendTask("task-suspend-2")
	if err == nil {
		t.Fatal("expected error when suspending non-running task, got nil")
	}
}

func TestSuspendTask_NotFound_ReturnsError(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	err := s.SuspendTask("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent task, got nil")
	}
}

func TestResumeSuspendedTask_SuspendedTask_GetsRunning(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-resume-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepPending, Task: "do work"},
		},
		Status: schemas.GraphSuspended,
	}

	injectTestGraph(t, s, graph)

	if err := s.ResumeSuspendedTask("task-resume-1"); err != nil {
		t.Fatalf("ResumeSuspendedTask failed: %v", err)
	}

	g := s.GetTaskGraph("task-resume-1")

	if g.Status != schemas.GraphRunning {
		t.Fatalf("expected status %q, got %q", schemas.GraphRunning, g.Status)
	}
}

func TestResumeSuspendedTask_NonSuspendedTask_ReturnsError(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-resume-2",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}

	injectTestGraph(t, s, graph)

	err := s.ResumeSuspendedTask("task-resume-2")
	if err == nil {
		t.Fatal("expected error when resuming non-suspended task, got nil")
	}
}

func TestResumeSuspendedTask_NotFound_ReturnsError(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	err := s.ResumeSuspendedTask("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent task, got nil")
	}
}

func TestSuspendThenResume_RoundTrip(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-roundtrip-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, Task: "do work"},
		},
		Status: schemas.GraphRunning,
	}

	injectRunningTestGraph(t, s, graph)

	// Suspend
	if err := s.SuspendTask("task-roundtrip-1"); err != nil {
		t.Fatalf("SuspendTask failed: %v", err)
	}
	if g := s.GetTaskGraph("task-roundtrip-1"); g.Status != schemas.GraphSuspended {
		t.Fatal("expected GraphSuspended after suspend")
	}

	// Resume
	if err := s.ResumeSuspendedTask("task-roundtrip-1"); err != nil {
		t.Fatalf("ResumeSuspendedTask failed: %v", err)
	}
	if g := s.GetTaskGraph("task-roundtrip-1"); g.Status != schemas.GraphRunning {
		t.Fatal("expected GraphRunning after resume")
	}
}

func TestResumeSuspendedTask_ResetsZombieSteps(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	// Steps have a circular dependency (s1→s2→s1) so that after resume,
	// neither step is "ready" per ReadySteps(). This prevents the graph
	// executor from immediately picking up s1 and setting it back to
	// StepRunning before the test can observe the reset to StepPending.
	graph := &schemas.TaskGraph{
		TaskID: "task-zombie-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, Task: "do work", DependsOn: []string{"s2"}},
			"s2": {ID: "s2", Status: schemas.StepPending, Task: "next work", DependsOn: []string{"s1"}},
		},
		Status: schemas.GraphSuspended,
	}

	injectTestGraph(t, s, graph)

	if err := s.ResumeSuspendedTask("task-zombie-1"); err != nil {
		t.Fatalf("ResumeSuspendedTask failed: %v", err)
	}

	g := s.GetTaskGraph("task-zombie-1")

	// StepRunning should have been reset to StepPending.
	if g.Steps["s1"].Status != schemas.StepPending {
		t.Fatalf("expected zombie step reset to %q, got %q", schemas.StepPending, g.Steps["s1"].Status)
	}
}
