package schemas

import (
	"encoding/json"
	"testing"
)

// Register O47 regression: task-to-SOP promotion must preserve audit intent but
// must not freeze deployment-owned verifier provider/model names into the SOP.
func TestSOPStepFromStep_PreservesIntentWithoutVerifierPolicy(t *testing.T) {
	step := &Step{
		ID: "s1", RoleID: "worker", Task: "t",
		RequireAudit: true, VerifierProvider: "audit-cloud", VerifierModel: "audit-model",
	}
	got := SOPStepFromStep(step)
	if !got.RequireAudit {
		t.Fatal("require_audit lost during SOP promotion")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || containsJSONKey(raw, "verifier_model") || containsJSONKey(raw, "verifier_provider") {
		t.Fatalf("resolved verifier policy leaked into SOP: %s", raw)
	}
}

func containsJSONKey(raw []byte, key string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	_, ok := fields[key]
	return ok
}
