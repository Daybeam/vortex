package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// -----------------------------------------------------------------------------
// Feature: Dynamic DAG Surgery & Dead-End Node Refactoring
// Scenario: the step being deprecated is what a *blocked* downstream step was
// blocked on, so the surgery removes the only reason for that block
// Source: docs/audit/unwired-code-register.md O22 (measured on task_e3e78311 /
// dec_f4f008: parallel_hunting deprecated, adversarial_verification still
// `blocked` with an empty DependsOn, run ended `failed` with output_files=[])
//
// Bug being pinned: mutateGraphTopologyLocked stripped the deprecated ID from
// every DependsOn but only ever looked at StepPending steps, and nothing in the
// engine moves StepBlocked back (schemas/task.go:308 counts it terminal), so a
// step that maybeBlock had blocked over the failed upstream stayed blocked
// forever. Answering `skip` marks the decision's own step only, which is how the
// state is reached in production.
// -----------------------------------------------------------------------------

func newSurgeryReviveEngine(t *testing.T) (*DirectedEngine, *schemas.TaskGraph) {
	t.Helper()
	tmpDir := mustTempDir(t, "surgery-revive")
	logger := mustNewLogger(t, tmpDir, nil)
	t.Cleanup(func() { logger.Close() })

	engine := &DirectedEngine{
		graphs:    make(map[string]*schemas.TaskGraph),
		logger:    logger,
		registry:  &config.Registry{System: config.SystemSettings{ConfidenceThreshold: 0.8}},
		doneChans: make(map[string]chan struct{}),
	}

	graph := &schemas.TaskGraph{
		TaskID: "task_revive",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			// The dead end being deprecated.
			"s_hunt": {ID: "s_hunt", Status: schemas.StepFailed, LastError: "sieve_intercepted: repetition detected"},
			// A step that failed for its own reasons and is NOT being deprecated.
			"s_other_dead_end": {ID: "s_other_dead_end", Status: schemas.StepFailed, LastError: "role missing"},
			"s_ok":             {ID: "s_ok", Status: schemas.StepOK},
			// Blocked by maybeBlock over s_hunt; the surgery removes that dep.
			"s_verify": {ID: "s_verify", DependsOn: []string{"s_hunt"}, Status: schemas.StepBlocked},
			// Same, but with one already-satisfied dep left over.
			"s_verify_multi": {ID: "s_verify_multi", DependsOn: []string{"s_ok", "s_hunt"}, Status: schemas.StepBlocked},
			// Blocked over a step this surgery does not touch: must stay blocked.
			"s_blocked_elsewhere": {ID: "s_blocked_elsewhere", DependsOn: []string{"s_other_dead_end"}, Status: schemas.StepBlocked},
			// Blocked and holding an unanswered hand-over: must stay blocked.
			"s_delegation": {ID: "s_delegation", DependsOn: []string{"s_hunt"}, Status: schemas.StepBlocked},
			// Blocked over a step that is itself still blocked: deps are not
			// satisfied, so it must wait for s_verify rather than jump the queue.
			"s_report": {ID: "s_report", DependsOn: []string{"s_verify"}, Status: schemas.StepBlocked},
		},
		PendingDecisions: []*schemas.Decision{
			{ID: "dec_delegation", StepID: "s_delegation", Type: schemas.DecisionDelegationRequired, Options: []string{"fulfill", "skip", "abort"}},
		},
		ContextTree: map[string]*schemas.ContextNode{
			"root": {ID: "root", StepIDs: []string{"s_hunt", "s_verify"}},
		},
		CurrentNodeID: "root",
	}
	engine.graphs["task_revive"] = graph
	return engine, graph
}

func TestMutateGraphTopology_RevivesStepsTheDeprecatedStepBlocked(t *testing.T) {
	engine, graph := newSurgeryReviveEngine(t)

	err := engine.MutateGraphTopology("task_revive", SurgeryData{
		DeprecatedStepID: "s_hunt",
		ReplacementSteps: []schemas.StepInput{
			{ID: "s_phase2", RoleID: "security_recon_mapper", Task: "Replacement path", DependsOn: []string{}},
		},
	})
	if err != nil {
		t.Fatalf("MutateGraphTopology failed: %v", err)
	}

	// Revived: blocked only by the deprecated step.
	for _, id := range []string{"s_verify", "s_verify_multi"} {
		if got := graph.Steps[id].Status; got != schemas.StepPending {
			t.Errorf("%s: expected pending after the block's cause was deprecated, got %s", id, got)
		}
		if graph.Steps[id].LastError != "" {
			t.Errorf("%s: a revived step returns to pending empty-handed like every other retry path, got LastError %q", id, graph.Steps[id].LastError)
		}
	}

	// The revived step is what ReadySteps was withholding it from.
	ready := map[string]bool{}
	for _, st := range graph.ReadySteps() {
		ready[st.ID] = true
	}
	if !ready["s_verify"] {
		t.Errorf("s_verify was revived but is still not in ReadySteps, so the run loop will never pick it up")
	}

	// Not revived, each for its own reason.
	if got := graph.Steps["s_blocked_elsewhere"].Status; got != schemas.StepBlocked {
		t.Errorf("s_blocked_elsewhere depends on a step this surgery does not deprecate; it must stay blocked, got %s", got)
	}
	if got := graph.Steps["s_delegation"].Status; got != schemas.StepBlocked {
		t.Errorf("s_delegation has an unanswered delegation decision; reviving it would run a step whose hand-over is still outstanding, got %s", got)
	}
	if got := graph.Steps["s_report"].Status; got != schemas.StepBlocked {
		t.Errorf("s_report's remaining dependency (s_verify) is not satisfied yet; it must stay blocked, got %s", got)
	}

	// The stripped deps are still stripped (unchanged behaviour, pinned so the
	// revival cannot be paid for by re-adding the deprecated ID).
	for _, id := range []string{"s_verify", "s_verify_multi", "s_delegation"} {
		for _, dep := range graph.Steps[id].DependsOn {
			if dep == "s_hunt" {
				t.Errorf("%s still depends on the deprecated s_hunt", id)
			}
		}
	}
	if got := len(graph.Steps["s_verify_multi"].DependsOn); got != 1 {
		t.Errorf("s_verify_multi: expected the satisfied s_ok dep to survive, got %d deps", got)
	}
}

// A pending step that never depended on the deprecated ID must not be revived by
// accident, and a pending step must keep the pre-existing log-only behaviour
// rather than be rewritten.
func TestMutateGraphTopology_LeavesPendingStepsAlone(t *testing.T) {
	engine, graph := newSurgeryReviveEngine(t)
	graph.Steps["s_verify"].Status = schemas.StepPending
	graph.Steps["s_pending_unrelated"] = &schemas.Step{ID: "s_pending_unrelated", Status: schemas.StepPending, DependsOn: []string{"s_other_dead_end"}}

	if err := engine.MutateGraphTopology("task_revive", SurgeryData{
		DeprecatedStepID: "s_hunt",
		ReplacementSteps: []schemas.StepInput{{ID: "s_phase2", RoleID: "r", Task: "t"}},
	}); err != nil {
		t.Fatalf("MutateGraphTopology failed: %v", err)
	}

	if got := graph.Steps["s_pending_unrelated"].Status; got != schemas.StepPending {
		t.Errorf("s_pending_unrelated: got %s", got)
	}
	for _, dep := range graph.Steps["s_pending_unrelated"].DependsOn {
		if dep == "s_hunt" {
			t.Errorf("a step that never depended on s_hunt had its deps rewritten")
		}
	}
}
