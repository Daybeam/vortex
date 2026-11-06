package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression for O28: Case 4 tested `fmt.Sprintf("%v", output.Result)`, and
// output.Result is a map[string]any, so the text under test was `map[k:v]` —
// never parseable as JSON. Every step declaring exit_criteria "json" was
// rejected by this gate no matter what its subagent returned; measured casualties
// are workspace/sops/video_drama_production_pipeline.json (step_storyboard,
// blocked at step 2 of 5) and sop_auto_generate.json.

func runJSONExitGate(t *testing.T, result map[string]any) (bool, string, schemas.VerificationFailureType) {
	t.Helper()
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	step := &schemas.Step{ID: "s1", ExitCriteria: "json"}
	graph := &schemas.TaskGraph{
		TaskID: "t-json-gate",
		Steps:  map[string]*schemas.Step{"s1": step},
	}
	return s.verifyExitCriteria(context.Background(), graph, step,
		&SpawnResult{Output: schemas.SubagentOutput{Result: result}})
}

// TestExitCriteriaJSON_StructuredResultPasses is the case that could never pass
// before O28 was fixed: a subagent that returned a JSON object lands in Result
// as a map, and the gate rendered it with %v.
func TestExitCriteriaJSON_StructuredResultPasses(t *testing.T) {
	ok, reason, failType := runJSONExitGate(t, map[string]any{
		"shots":   []any{map[string]any{"id": float64(1), "shot": "wide"}},
		"summary": "12 shots",
	})
	if !ok {
		t.Errorf("structured result must satisfy \"json\", got reason %q (failType %q)", reason, failType)
	}
}

// TestExitCriteriaJSON_ProseFallbackStillRejected keeps the gate from becoming a
// guard that cannot fail: when ParseOutput could not find JSON at all it stores
// the raw text under "raw" (schemas/output.go:370), and that is the shape the
// criterion exists for.
func TestExitCriteriaJSON_ProseFallbackStillRejected(t *testing.T) {
	ok, reason, failType := runJSONExitGate(t, map[string]any{
		"raw": "I could not produce a storyboard because the scene list was empty.",
	})
	if ok {
		t.Fatal("prose must not satisfy \"json\"")
	}
	if reason != "output is not valid JSON" || failType != schemas.FailureSchemaViolation {
		t.Errorf("expected the schema-violation rejection, got %q / %q", reason, failType)
	}
}

// TestExitCriteriaJSON_JSONTextUnderRawPasses: valid JSON delivered as a string
// is still JSON.
func TestExitCriteriaJSON_JSONTextUnderRawPasses(t *testing.T) {
	ok, reason, _ := runJSONExitGate(t, map[string]any{
		"raw": `{"shots": [{"id": 1}], "summary": "12 shots"}`,
	})
	if !ok {
		t.Errorf("valid JSON text under \"raw\" must pass, got %q", reason)
	}
}

// TestExitCriteriaJSON_TruncatedTextRepairs pins the branch that truncation
// repair was written for. Repair operates on model text, so it only ever had a
// chance to run once the gate stopped reading `map[...]`.
func TestExitCriteriaJSON_TruncatedTextRepairs(t *testing.T) {
	ok, reason, _ := runJSONExitGate(t, map[string]any{
		"raw": `{"shots": [{"id": 1}, {"id": 2`,
	})
	if !ok {
		t.Errorf("truncated JSON should repair, got %q", reason)
	}
}

// TestExitCriteriaJSON_EmptyResultFails: no payload is not "valid JSON". This is
// the one rejection the pre-fix gate reached by accident, kept on purpose.
func TestExitCriteriaJSON_EmptyResultFails(t *testing.T) {
	ok, _, _ := runJSONExitGate(t, map[string]any{})
	if ok {
		t.Error("an empty result must not satisfy \"json\"")
	}
}
