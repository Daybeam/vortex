package core

import (
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Regression for O38: every parked decision type needs stale visibility, but
// reporting must never resolve, expire or mutate it. Delegation keeps its
// existing 30-minute report; other decisions use the configurable 24-hour window.
func TestDecisionStale_AllTypesAreVisibleWithoutMutation(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "o38_all_types"
	graph := newFailedDecisionGraph(taskID, 25*time.Hour)
	graph.PendingDecisions = append(graph.PendingDecisions,
		&schemas.Decision{ID: "dec_cap", StepID: "s1", Type: schemas.DecisionCapabilityRequired, Created: time.Now().Add(-26 * time.Hour)},
		&schemas.Decision{ID: "dec_approval", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired, Created: time.Now().Add(-23 * time.Hour)},
		&schemas.Decision{ID: "dec_deleg", StepID: "s2", Type: schemas.DecisionDelegationRequired, Created: time.Now().Add(-31 * time.Minute)},
	)
	injectTestGraph(t, s, graph)

	status := mustStatus(t, s, taskID)
	raw, ok := status["decision_stale"].(map[string]any)
	if !ok {
		t.Fatalf("decision_stale missing: %#v", status)
	}
	if raw["stale"] != true || raw["oldest_wait_seconds"].(int) < 26*3600 {
		t.Fatalf("decision_stale summary = %#v", raw)
	}
	decisions, ok := raw["decisions"].([]StaleDecision)
	if !ok || len(decisions) != 3 {
		t.Fatalf("decisions = %#v, want three stale types", raw["decisions"])
	}
	byID := make(map[string]StaleDecision)
	for _, d := range decisions {
		byID[d.DecisionID] = d
	}
	if byID["dec_deleg"].Type != schemas.DecisionDelegationRequired || byID["dec_deleg"].TimeoutSeconds != 1800 {
		t.Errorf("delegation window = %#v", byID["dec_deleg"])
	}
	if byID["dec_cap"].Type != schemas.DecisionCapabilityRequired || byID["dec_cap"].TimeoutSeconds != 86400 {
		t.Errorf("generic window = %#v", byID["dec_cap"])
	}
	if _, included := byID["dec_approval"]; included {
		t.Error("23-hour approval is inside the 24-hour window")
	}
	if !strings.Contains(raw["error"].(string), "report, not an abort") {
		t.Errorf("error does not state non-mutation: %q", raw["error"])
	}
	if waitReport(t, status) == nil || staleReport(t, status) == nil {
		t.Error("existing typed reports must remain available")
	}
	if graph.Status != schemas.GraphRunning || len(graph.PendingDecisions) != 4 || graph.Steps["s1"].Status != schemas.StepBlocked {
		t.Fatalf("stale reporting mutated graph: %#v", graph)
	}
}

func TestDecisionStale_RespectsIndependentTimeoutControls(t *testing.T) {
	cases := []struct {
		name       string
		generic    *int
		delegation *int
		want       bool
	}{
		{"defaults", nil, nil, true},
		{"generic_disabled_delegation_unaffected", staleTimeoutPtr(0), nil, true},
		{"delegation_disabled", nil, staleTimeoutPtr(0), false},
		{"both_disabled", staleTimeoutPtr(-1), staleTimeoutPtr(0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerTestEngine(t)
			s.registry.System.DecisionStaleTimeoutSeconds = tc.generic
			s.registry.System.DelegationAnswerTimeoutSeconds = tc.delegation
			taskID := "o38_" + tc.name
			graph := newFailedDecisionGraph(taskID, 25*time.Hour)
			graph.PendingDecisions = []*schemas.Decision{{
				ID: "dec_deleg", StepID: "s1", Type: schemas.DecisionDelegationRequired,
				Created: time.Now().Add(-31 * time.Minute),
			}}
			injectTestGraph(t, s, graph)
			status := mustStatus(t, s, taskID)
			_, got := status["decision_stale"]
			if got != tc.want {
				t.Fatalf("decision_stale present=%v, want %v", got, tc.want)
			}
		})
	}
}
