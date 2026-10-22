package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression (HIGH-sev, 2026-10-21): TaskGraph.SubmitterIsAdmin must survive
// JSON marshal/unmarshal so fork / CaSKG / SQLite-reload / crash-recovery
// retain the submitter tier. Previously the field was json:"-", which dropped
// it on marshal — resumed or forked admin tasks lost admin and their dynamic
// MCP mounts were rejected mid-task.
func TestTaskGraph_SubmitterIsAdmin_PersistsAcrossJSON(t *testing.T) {
	g := &TaskGraph{TaskID: "t1", SubmitterIsAdmin: true}
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var g2 TaskGraph
	if err := json.Unmarshal(data, &g2); err != nil {
		t.Fatal(err)
	}
	if !g2.SubmitterIsAdmin {
		t.Fatal("SubmitterIsAdmin lost across marshal/unmarshal — fork/reload would downgrade admin tasks")
	}

	// Public tier round-trips as false.
	d2, _ := json.Marshal(&TaskGraph{TaskID: "t2"})
	var gP TaskGraph
	json.Unmarshal(d2, &gP)
	if gP.SubmitterIsAdmin {
		t.Fatal("public graph must not become admin")
	}
}

// StepInput.SubmitterIsAdmin must NOT be wire-visible: a caller submitting
// steps cannot forge admin (only the entry handler sets it from ctx).
func TestStepInput_SubmitterIsAdmin_NotWireVisible(t *testing.T) {
	data, _ := json.Marshal(StepInput{ID: "s1", SubmitterIsAdmin: true})
	if strings.Contains(string(data), "submitter_is_admin") {
		t.Fatalf("StepInput.SubmitterIsAdmin leaked to wire: %s", data)
	}
	var in StepInput
	if err := json.Unmarshal([]byte(`{"id":"s1","submitter_is_admin":true}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.SubmitterIsAdmin {
		t.Fatal("caller must not be able to forge StepInput.SubmitterIsAdmin via JSON")
	}
}
