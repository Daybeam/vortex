package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestAddDecision_PublicTierNeverOfferedAdminOnlyOptions is the regression test
// for AUTH-3 / G9 (2026-10-23): the engine must not offer a decision option the
// submitter's tier cannot choose. validateTierChoice (tools/tools_task.go)
// rejects an admin-only choice from a public caller at submit time; emitting it
// anyway showed the caller a choice that would fail.
func TestAddDecision_PublicTierNeverOfferedAdminOnlyOptions(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	adminOnly := []string{"retry_with_context", "retry_with_skill:foo", "escalate_model"}
	mixed := append([]string{"skip", "abort", "retry"}, adminOnly...)

	// ── Public submitter: admin-only options must be filtered out. ──
	pubGraph := &schemas.TaskGraph{
		TaskID:           "t_pub",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
		SubmitterIsAdmin: false,
	}
	pubStep := &schemas.Step{ID: "s1", Status: schemas.StepBlocked}
	pubGraph.Steps["s1"] = pubStep

	engine.addDecision(pubGraph, pubStep, schemas.DecisionStepFailed,
		map[string]any{"root_cause": "execution_failed"}, append([]string{}, mixed...))

	if len(pubGraph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(pubGraph.PendingDecisions))
	}
	pubOpts := pubGraph.PendingDecisions[0].Options
	for _, o := range pubOpts {
		if !publicTierOptions[o] {
			t.Errorf("public-tier decision offered admin-only option %q (options=%v)", o, pubOpts)
		}
	}
	for _, forbidden := range adminOnly {
		for _, o := range pubOpts {
			if o == forbidden {
				t.Errorf("admin-only option %q leaked into a public-tier decision (%v)", forbidden, pubOpts)
			}
		}
	}

	// ── Admin submitter: the full set is preserved. ──
	admGraph := &schemas.TaskGraph{
		TaskID:           "t_adm",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
		SubmitterIsAdmin: true,
	}
	admStep := &schemas.Step{ID: "s1", Status: schemas.StepBlocked}
	admGraph.Steps["s1"] = admStep

	engine.addDecision(admGraph, admStep, schemas.DecisionStepFailed,
		map[string]any{"root_cause": "execution_failed"}, append([]string{}, mixed...))

	if len(admGraph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(admGraph.PendingDecisions))
	}
	found := false
	for _, o := range admGraph.PendingDecisions[0].Options {
		if o == "retry_with_context" {
			found = true
		}
	}
	if !found {
		t.Fatalf("admin-tier decision must keep admin-only options, got %v", admGraph.PendingDecisions[0].Options)
	}
}
