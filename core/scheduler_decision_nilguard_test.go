package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// audit L8 (2026-11-05): regression test for addDecision nil guards.
// addDecision dereferences step.ID, step.Task, and step.AdditionalMCPs
// unconditionally, but formerly had a contradictory "if step != nil"
// guard at the resultRef line. The fix adds an explicit early return
// for nil graph or nil step so a future nil caller fails safe instead
// of panicking. All current callers pass non-nil step (verified by
// caller audit), but the guard prevents a latent panic.

func TestAddDecision_NilGraphNoPanic(t *testing.T) {
	engine := &DirectedEngine{}
	// Must not panic with nil graph.
	engine.addDecision(nil, &schemas.Step{ID: "s1"}, schemas.DecisionStepFailed, nil, nil)
}

func TestAddDecision_NilStepNoPanic(t *testing.T) {
	engine := &DirectedEngine{}
	graph := &schemas.TaskGraph{Steps: map[string]*schemas.Step{}}
	// Must not panic with nil step.
	engine.addDecision(graph, nil, schemas.DecisionStepFailed, nil, nil)
}

func TestAddDecision_BothNilNoPanic(t *testing.T) {
	engine := &DirectedEngine{}
	// Must not panic with both nil.
	engine.addDecision(nil, nil, schemas.DecisionStepFailed, nil, nil)
}
