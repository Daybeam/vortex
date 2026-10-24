package contract_tests

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestMCPContract_SubmitTask_ReportsAppendedVerifierStep is the regression test
// for Issue-3 (2026-10-23). When the Adaptive Verifier Policy appends an
// auditor step for a critical-tier task, the response must report the caller's
// ORIGINAL step count separately from the final DAG size. Previously only the
// final step_count was returned, so "1 submitted -> 2 in DAG" read like a
// failed submit and could cause a duplicate submission.
func TestMCPContract_SubmitTask_ReportsAppendedVerifierStep(t *testing.T) {
	// Deterministic trigger: pin the critical keyword so the test does not
	// depend on the default list drifting (riskTierKeywords is a package
	// global). Restore the defaults afterwards.
	defaultCritical := []string{"browser", "image", "generate", "scrape", "screenshot", "pollinations", "deploy", "payment"}
	defaultModerate := []string{"patch", "refactor", "build", "test"}
	defer schemas.SetRiskTierKeywords(defaultCritical, defaultModerate)
	schemas.SetRiskTierKeywords([]string{"zzz_verify_trigger"}, nil)

	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	res, err := callTool(t, s, "orchestrator_submit_task", map[string]any{
		"task": "Handle the zzz_verify_trigger case.",
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected success for a critical-tier task, got error")
	}

	out := parseResultJSON(t, res)

	submitted, ok := out["submitted_step_count"].(float64)
	if !ok {
		t.Fatalf("submitted_step_count missing or wrong type (got %T %v)", out["submitted_step_count"], out["submitted_step_count"])
	}
	if submitted != 1 {
		t.Fatalf("expected submitted_step_count=1, got %v", submitted)
	}

	added, ok := out["added_step_count"].(float64)
	if !ok {
		t.Fatalf("added_step_count missing or wrong type (got %T %v)", out["added_step_count"], out["added_step_count"])
	}
	if added < 1 {
		t.Fatalf("expected added_step_count>=1 (auditor appended for a critical step), got %v", added)
	}

	final, ok := out["step_count"].(float64)
	if !ok {
		t.Fatalf("step_count missing or wrong type (got %T)", out["step_count"])
	}
	if final != submitted+added {
		t.Fatalf("step_count (%v) must equal submitted_step_count (%v) + added_step_count (%v)", final, submitted, added)
	}

	// The appended step must be discoverable so an agent can correlate it.
	av, ok := out["adaptive_verifier"].([]any)
	if !ok || len(av) == 0 {
		t.Fatalf("expected adaptive_verifier to list the appended step, got %T %v", out["adaptive_verifier"], out["adaptive_verifier"])
	}
}