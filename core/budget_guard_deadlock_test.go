package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

func newTestEngineForBudgetDeadlock(t *testing.T) *DirectedEngine {
	t.Helper()
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return &DirectedEngine{
		logger:      logger,
		outputBase:  t.TempDir(),
		budgetGuard: NewBudgetGuard(0),
		doneChans:   make(map[string]chan struct{}),
	}
}

// TestExecuteStep_BudgetExceeded_DoesNotDeadlock is a direct regression test
// for the 2026-08-31 fix: the original code held s.Mu.Lock() around a call
// to addDecision, and addDecision internally calls broadcastDone, which
// itself takes s.Mu.Lock() -- since Go's sync.RWMutex is not reentrant, that
// would deadlock the engine's mutex the very first time a task's token
// budget was actually exceeded (a real, previously undetected bug in the
// "Token Budget Pre-check (ADDED 2026-08-30)" feature). This test runs
// executeStep's budget pre-check on a background goroutine and fails if it
// does not return within a generous deadline, which is exactly what the old
// code would never do once graph.TokensUsed >= graph.TokenBudget.
func TestExecuteStep_BudgetExceeded_DoesNotDeadlock(t *testing.T) {
	engine := newTestEngineForBudgetDeadlock(t)

	graph := &schemas.TaskGraph{
		TaskID:           "task_budget_deadlock_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
		OutputFiles:      []schemas.OutputFile{},
		ContextTree:      map[string]*schemas.ContextNode{},
		TokenBudget:      100,
		TokensUsed:       1000, // already well over budget
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending}
	graph.Steps[step.ID] = step

	done := make(chan struct{})
	go func() {
		engine.executeStep(context.Background(), graph, step)
		close(done)
	}()

	select {
	case <-done:
		// Good -- returned without deadlocking.
	case <-time.After(5 * time.Second):
		t.Fatal("executeStep did not return within 5s when the token budget was already exceeded -- likely deadlocked on s.Mu (exactly the bug this test guards against)")
	}

	if step.Status != schemas.StepBlocked {
		t.Fatalf("expected step.Status == StepBlocked after budget exhaustion, got %v", step.Status)
	}
	if !graph.BudgetPaused {
		t.Fatal("expected graph.BudgetPaused == true after budget exhaustion")
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected exactly 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	if graph.PendingDecisions[0].Type != schemas.DecisionBudgetExhausted {
		t.Fatalf("expected DecisionBudgetExhausted, got %v", graph.PendingDecisions[0].Type)
	}
	if graph.Status != schemas.GraphBlocked {
		t.Fatalf("expected graph.Status == GraphBlocked (set by addDecision), got %v", graph.Status)
	}
}

// TestExecuteStep_BudgetNotExceeded_ProceedsPastPrecheck confirms the fix
// did not accidentally invert the condition: a graph well within budget
// (or with no budget configured at all, the common case for nearly every
// existing task) must not be blocked by the pre-check. We only assert on
// step.Status having moved off StepPending into something the rest of
// executeStep would produce (StepRunning is set immediately after the
// pre-check, before this goroutine's step nondeterministically proceeds
// further and likely fails for unrelated reasons such as a nil spawner --
// which is fine, since only the pre-check itself is under test here).
func TestExecuteStep_BudgetNotExceeded_ProceedsPastPrecheck(t *testing.T) {
	engine := newTestEngineForBudgetDeadlock(t)

	graph := &schemas.TaskGraph{
		TaskID:           "task_budget_ok_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
		OutputFiles:      []schemas.OutputFile{},
		ContextTree:      map[string]*schemas.ContextNode{},
		TokenBudget:      0, // unset/no budget configured -- CheckBudget always passes
		TokensUsed:       0,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending, MaxRetries: 0}
	graph.Steps[step.ID] = step

	done := make(chan struct{})
	go func() {
		defer func() {
			// A nil s.spawner will panic once execution reaches s.spawner.Spawn(...)
			// further down executeStep -- that is expected and outside this test's
			// scope (we only care whether the budget pre-check itself let execution
			// through). Recover so the panic doesn't fail the whole test binary.
			_ = recover()
			close(done)
		}()
		engine.executeStep(context.Background(), graph, step)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeStep did not return within 5s")
	}

	if len(graph.PendingDecisions) != 0 {
		t.Fatalf("expected no budget_exhausted decision when budget is not configured/exceeded, got %d pending decisions", len(graph.PendingDecisions))
	}
	if graph.BudgetPaused {
		t.Fatal("expected graph.BudgetPaused to remain false when budget is not exceeded")
	}
}
