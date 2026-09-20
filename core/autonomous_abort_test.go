package core

import (
	"context"
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func newAbortTestEngine(t *testing.T) (*DirectedEngine, *config.Registry) {
	t.Helper()
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logDir, _ := os.MkdirTemp("", "abort-log-*")
	outDir, _ := os.MkdirTemp("", "abort-out-*")
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	return s, reg
}

func TestRequestAutonomousAbort_CreatesDecision(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-abort-1",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-abort-1"] = graph
	s.Mu.Unlock()

	s.RequestAutonomousAbort("task-abort-1", "s1", FailureClassCostOverrun, "cache 0% for 5 consecutive rounds")

	s.Mu.RLock()
	g := s.graphs["task-abort-1"]
	s.Mu.RUnlock()

	if len(g.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(g.PendingDecisions))
	}
	dec := g.PendingDecisions[0]
	if dec.Type != schemas.DecisionAutonomousAbortRequested {
		t.Fatalf("expected decision type %s, got %s", schemas.DecisionAutonomousAbortRequested, dec.Type)
	}
	if fc, _ := dec.Context["failure_class"].(string); fc != string(FailureClassCostOverrun) {
		t.Fatalf("expected failure_class %s, got %s", FailureClassCostOverrun, fc)
	}
	hasConfirm, hasContinue := false, false
	for _, opt := range dec.Options {
		if opt == "confirm_abort" {
			hasConfirm = true
		}
		if opt == "continue" {
			hasContinue = true
		}
	}
	if !hasConfirm || !hasContinue {
		t.Fatalf("expected options [confirm_abort, continue], got %v", dec.Options)
	}
}

func TestSubmitDecision_ConfirmAbort_CancelsTask(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-abort-2",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	ctx, cancel := context.WithCancel(context.Background())
	s.Mu.Lock()
	s.graphs["task-abort-2"] = graph
	s.cancelFuncs["task-abort-2"] = cancel
	s.Mu.Unlock()

	s.RequestAutonomousAbort("task-abort-2", "s1", FailureClassCostOverrun, "budget trajectory exceeds limit")

	decID := graph.PendingDecisions[0].ID

	if err := s.SubmitDecision("task-abort-2", decID, "confirm_abort"); err != nil {
		t.Fatalf("SubmitDecision failed: %v", err)
	}

	if graph.Status != schemas.GraphFailed {
		t.Fatalf("expected graph status GraphFailed, got %s", graph.Status)
	}
	if !graph.BudgetPaused {
		t.Fatal("expected BudgetPaused=true after confirm_abort")
	}

	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel func should have been called")
	}
}

func TestSubmitDecision_Continue_ResumesTask(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-abort-3",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-abort-3"] = graph
	s.Mu.Unlock()

	s.RequestAutonomousAbort("task-abort-3", "s1", FailureClassCostOverrun, "false alarm")
	decID := graph.PendingDecisions[0].ID

	if err := s.SubmitDecision("task-abort-3", decID, "continue"); err != nil {
		t.Fatalf("SubmitDecision failed: %v", err)
	}

	if step.Status != schemas.StepPending {
		t.Fatalf("expected step status StepPending after continue, got %s", step.Status)
	}
}

func TestRequestAutonomousAbort_NilGraph_NoPanic(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("should not panic on nil graph: %v", r)
		}
	}()
	s.RequestAutonomousAbort("nonexistent", "s1", FailureClassCostOverrun, "test")
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
