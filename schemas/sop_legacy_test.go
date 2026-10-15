package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSOP_UnmarshalLegacy(t *testing.T) {
	legacyJSON := `{
  "id": "legacy_sop",
  "version": "1.0",
  "description": "Legacy format with object triggers and slice steps",
  "triggers": {
    "keywords": ["test", "legacy"]
  },
  "steps": [
    {
      "id": "step1",
      "role": "tester",
      "task": "Do legacy test"
    }
  ]
}`

	var sop SOP
	if err := json.Unmarshal([]byte(legacyJSON), &sop); err != nil {
		t.Fatalf("failed to unmarshal legacy SOP: %v", err)
	}

	if len(sop.Triggers) != 1 {
		t.Errorf("expected 1 trigger, got %d", len(sop.Triggers))
	} else if sop.Triggers[0].Keywords[0] != "test" {
		t.Errorf("expected trigger keyword 'test', got %s", sop.Triggers[0].Keywords[0])
	}

	if len(sop.Steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(sop.Steps))
	} else if step, ok := sop.Steps["step1"]; !ok {
		t.Error("expected step 'step1' to exist")
	} else if step.Task != "Do legacy test" {
		t.Errorf("expected task 'Do legacy test', got %s", step.Task)
	}
}

// TestSOP_UnmarshalJSON_BadTriggerType is a regression test for audit L-15-3:
// UnmarshalJSON previously ignored json.Unmarshal errors for triggers/steps,
// silently dropping malformed entries. After the fix, it returns an error.
//
// Before fix: json.Unmarshal returns nil, sop.Triggers has 1 empty trigger.
// After fix:  json.Unmarshal returns an error mentioning "parse SOP trigger".
func TestSOP_UnmarshalJSON_BadTriggerType(t *testing.T) {
	// keywords is a number, but SOPTrigger.Keywords is []string — type mismatch.
	badJSON := `{"id":"bad","triggers":{"keywords":123},"steps":{}}`
	var sop SOP
	err := json.Unmarshal([]byte(badJSON), &sop)
	if err == nil {
		t.Fatal("expected error for trigger with wrong type, got nil (audit L-15-3: error was silently ignored)")
	}
	if !strings.Contains(err.Error(), "parse SOP trigger") {
		t.Errorf("expected error mentioning 'parse SOP trigger', got: %v", err)
	}
}

// TestSOP_UnmarshalJSON_BadStepsType is a regression test for audit L-15-3:
// steps as a map with a field of the wrong type should return an error
// instead of silently producing an empty steps map.
func TestSOP_UnmarshalJSON_BadStepsType(t *testing.T) {
	// steps is a map, but the step's "id" field is a number, not a string.
	badJSON := `{"id":"bad","triggers":[],"steps":{"s1":{"id":123,"role":"r","task":"t"}}}`
	var sop SOP
	err := json.Unmarshal([]byte(badJSON), &sop)
	if err == nil {
		t.Fatal("expected error for steps with wrong type, got nil (audit L-15-3: error was silently ignored)")
	}
	if !strings.Contains(err.Error(), "parse SOP steps") {
		t.Errorf("expected error mentioning 'parse SOP steps', got: %v", err)
	}
}
