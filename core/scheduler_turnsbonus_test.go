package core

import (
	"fmt"
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestSubmitDecision_TurnsBudgetBonusCapped is a regression test for audit H3:
// resume_more_turns used to do `step.TurnsBudgetBonus += chunk` with no upper
// bound, so a stuck step that was repeatedly resumed accumulated unbounded
// turn budget (bypassing BudgetGuard and burning unbounded tokens). After the
// fix the bonus is capped at 4× the base chunk.
func TestSubmitDecision_TurnsBudgetBonusCapped(t *testing.T) {
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{MaxToolTurns: 50},
	}
	logDir, _ := os.MkdirTemp("", "h3-log-*")
	outDir, _ := os.MkdirTemp("", "h3-out-*")
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	defer s.Stop()

	const taskID = "task-h3"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step
	s.Mu.Lock()
	s.graphs[taskID] = graph
	s.Mu.Unlock()

	// Resume the same step far past the cap. Each call needs a fresh pending
	// decision (SubmitDecision removes it after processing).
	for i := 0; i < 100; i++ {
		decID := fmt.Sprintf("dec-%d", i)
		s.Mu.Lock()
		graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
			ID:     decID,
			StepID: "s1",
			Type:   schemas.DecisionMaxTurnsExhausted,
		})
		s.Mu.Unlock()
		if err := s.SubmitDecision(taskID, decID, "resume_more_turns"); err != nil {
			t.Fatalf("SubmitDecision #%d: %v", i, err)
		}
	}

	// chunk = MaxToolTurns = 50; cap = 4*chunk = 200. Without the cap this
	// would be 100*50 = 5000.
	if want := 200; step.TurnsBudgetBonus != want {
		t.Fatalf("TurnsBudgetBonus = %d, want capped at %d (uncapped would be 5000)", step.TurnsBudgetBonus, want)
	}
}
