package schemas

import (
	"encoding/json"
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
