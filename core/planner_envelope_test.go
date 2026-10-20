package core

import (
	"encoding/json"
	"testing"
)

// The fixtures below are the shapes actually observed, not invented ones:
// gemini-3-flash answered the planner prompt with the engine's own subagent
// envelope, and the spawner had already wrapped that once, so the text that
// reached ParsePlanDAG was {"raw":"<envelope as escaped string>"}.
const observedPlannerOutput = `{"raw":"{\"status\": \"ok\", \"confidence\": 1.0, \"result\": {\"steps\": [{\"id\": \"s1\", \"role_id\": \"appworld_agent\", \"task\": \"find\", \"allowed_tools\": [\"phone__login\", \"phone__get_current_date_and_time\"]}]}}"}`

func TestUnwrapPlanEnvelope_ObservedDoubleWrappedOutput(t *testing.T) {
	got := unwrapPlanEnvelope(observedPlannerOutput)

	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("unwrapped output is not JSON: %v\n%s", err, got)
	}
	steps, ok := obj["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("expected 1 step after unwrapping, got %v", obj["steps"])
	}
	first := steps[0].(map[string]any)
	if first["role_id"] != "appworld_agent" {
		t.Fatalf("step payload was mangled: %v", first)
	}
}

// TestUnwrapPlanEnvelope_LeavesBarePlanAlone pins the stop condition: an object
// that already has "steps" must never be re-interpreted, even if one of its
// step payloads contains a key that looks like a wrapper.
func TestUnwrapPlanEnvelope_LeavesBarePlanAlone(t *testing.T) {
	bare := `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"x","allowed_tools":[],"result":"ignored"}]}`
	if got := unwrapPlanEnvelope(bare); got != bare {
		t.Fatalf("a bare plan must pass through unchanged, got %s", got)
	}
}

// TestUnwrapPlanEnvelope_SingleResultWrapper covers the one-level shape, which
// is what a model emits when it copies the engine's envelope once.
func TestUnwrapPlanEnvelope_SingleResultWrapper(t *testing.T) {
	raw := `{"status":"ok","confidence":1.0,"result":{"steps":[{"id":"s1"}]}}`
	got := unwrapPlanEnvelope(raw)
	if got != `{"steps":[{"id":"s1"}]}` {
		t.Fatalf("expected the inner plan, got %s", got)
	}
}

// TestUnwrapPlanEnvelope_PassesThroughNonJSON keeps non-JSON (markdown-fenced
// or prose) output intact so the existing extractor can still find the object.
func TestUnwrapPlanEnvelope_PassesThroughNonJSON(t *testing.T) {
	raw := "```json\n{\"steps\": []}\n```"
	if got := unwrapPlanEnvelope(raw); got != raw {
		t.Fatalf("non-JSON must pass through unchanged, got %s", got)
	}
}
