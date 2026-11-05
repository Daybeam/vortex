package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression tests for Q4-5 (docs/plan/O_BATCH_REMEDIATION_2026-11-04.md,
// answer landed 2026-11-05): submit_decision must be able to route by the
// decision's own type instead of by whether the caller happened to attach a
// payload. That routing needs a read of the parked decision's type, which is
// what PendingDecisionType provides. O17 is the bug this unblocks: an
// answer of the form {choice:"abort", output:"{}"} used to be indistinguishable
// from a delegation hand-back.

func TestPendingDecisionType_ResolvesParkedDecisions(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q45_type_lookup"
	graph := newBlockedGraph(taskID) // dec_o25 is a step_failed decision
	graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
		ID:      "dec_deleg",
		StepID:  "s1",
		Type:    schemas.DecisionDelegationRequired,
		Options: []string{"fulfill"},
	})
	s.graphs[taskID] = graph

	tests := []struct {
		name       string
		decisionID string
		wantType   schemas.DecisionType
		wantOK     bool
	}{
		{"step_failed decision resolves", "dec_o25", schemas.DecisionStepFailed, true},
		{"delegation decision resolves", "dec_deleg", schemas.DecisionDelegationRequired, true},
		// The fallback contract: an id that is not parked must report not-ok,
		// so the tool layer keeps its payload-presence rule instead of
		// inventing a type.
		{"unknown decision does not resolve", "dec_nope", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.PendingDecisionType(taskID, tc.decisionID)
			if ok != tc.wantOK || got != tc.wantType {
				t.Fatalf("PendingDecisionType(%q, %q) = %q, %v; want %q, %v",
					taskID, tc.decisionID, got, ok, tc.wantType, tc.wantOK)
			}
		})
	}

	if _, ok := s.PendingDecisionType("task_that_does_not_exist", "dec_o25"); ok {
		t.Fatal("unknown task resolved: the tool layer would route on a type it never read")
	}
}

// A graph evicted from s.graphs is still answerable — FulfillStep reloads it
// from manifest.json, so the routing read must do the same. Otherwise a
// restarted engine would route every payload-carrying answer by the fallback
// rule and re-create O17 for exactly the tasks the reload path exists for.
func TestPendingDecisionType_ResolvesFromPersistedManifest(t *testing.T) {
	s, outputBase := newAnswerTestEngine(t)
	taskID := "q45_manifest_only"
	manifestPath := filepath.Join(outputBase, taskID, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := json.Marshal(newBlockedGraph(taskID))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, cached := s.graphs[taskID]; cached {
		t.Fatal("test setup: graph is already cached, so nothing was reloaded")
	}
	got, ok := s.PendingDecisionType(taskID, "dec_o25")
	if !ok || got != schemas.DecisionStepFailed {
		t.Fatalf("PendingDecisionType on a manifest-only task = %q, %v; want %q, true", got, ok, schemas.DecisionStepFailed)
	}
}
