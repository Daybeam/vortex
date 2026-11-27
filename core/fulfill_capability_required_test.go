package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression for O55: delegation fulfillment used to discard
// required_capabilities and force StepOK. A host that cannot finish the step
// must instead receive an actionable capability decision.
func TestFulfillStep_CapabilityRequiredParksAdvisoryDecision(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "capability_required_status",
			payload: `{"status":"capability_required","confidence":0.4,"result":{"need":"browser"},"required_capabilities":["browser","browser"]}`,
		},
		{
			name:    "required_capabilities_with_ok_status",
			payload: `{"status":"ok","confidence":0.9,"result":{"need":"browser"},"required_capabilities":["browser"]}`,
		},
		{
			name:    "required_capabilities_with_failed_status",
			payload: `{"status":"failed","confidence":0.4,"result":{},"required_capabilities":["browser"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, _ := newDelegationEngine(t)
			taskID, err := engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "worker", Task: "Use a browser"}})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			graph := waitForBlocked(t, engine, taskID)
			original := engine.FirstPendingDecision(taskID)
			if original == nil || original.Type != schemas.DecisionDelegationRequired {
				t.Fatalf("pending decision = %#v, want delegation", original)
			}
			engine.Mu.Lock()
			graph.SubmitterIsAdmin = true
			engine.Mu.Unlock()

			if err := engine.FulfillStep(taskID, original.ID, tt.payload); err != nil {
				t.Fatalf("FulfillStep: %v", err)
			}

			graph = engine.GetTaskGraph(taskID)
			if graph == nil || len(graph.PendingDecisions) != 1 {
				t.Fatalf("pending decisions = %#v, want exactly one replacement decision", graph)
			}
			dec := graph.PendingDecisions[0]
			if dec.ID == original.ID {
				t.Fatal("capability decision reused the consumed delegation decision ID")
			}
			if dec.Type != schemas.DecisionStepFailed {
				t.Fatalf("decision type = %q, want step_failed", dec.Type)
			}
			if got := dec.Context["root_cause"]; got != string(FailureClassCapabilityRequired) {
				t.Fatalf("root_cause = %v, want capability_required", got)
			}
			advisory, ok := dec.Context["capability_advisory"].(map[string]any)
			if !ok {
				t.Fatalf("capability_advisory = %#v, want object", dec.Context["capability_advisory"])
			}
			discover, _ := advisory["discover"].(string)
			for _, want := range []string{`subsystem="discovery"`, `action="discover"`, `scope="pool"`, `query="browser"`} {
				if !strings.Contains(discover, want) {
					t.Errorf("discover advisory %q does not contain %q", discover, want)
				}
			}
			invoke, _ := advisory["invoke"].(string)
			for _, want := range []string{"orchestrator_invoke", `subsystem="proxy"`, `action="call"`} {
				if !strings.Contains(invoke, want) {
					t.Errorf("invoke advisory %q does not contain %q", invoke, want)
				}
			}
			if !decisionHasOption(dec.Options, "retry_with_skill:browser") {
				t.Errorf("options = %v, want secondary retry_with_skill:browser", dec.Options)
			}
			if !decisionHasOption(dec.Options, "rewrite_dag") {
				t.Errorf("options = %v, want rewrite_dag", dec.Options)
			}

			step := graph.Steps["s1"]
			if step == nil || step.Status != schemas.StepBlocked {
				t.Fatalf("step = %#v, want blocked rather than StepOK", step)
			}
			if !strings.Contains(step.LastError, "capability_required") {
				t.Errorf("LastError = %q, want capability_required", step.LastError)
			}
			res, _ := engine.taskStore.Get(context.Background(), taskID, "s1")
			if res != nil {
				t.Fatalf("capability-required fulfillment was stored as a successful result: %#v", res)
			}
		})
	}
}

func TestFulfillStep_CapabilityAdvisoryRespectsPublicTier(t *testing.T) {
	engine, _ := newDelegationEngine(t)
	taskID, err := engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "worker", Task: "Use a browser"}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForBlocked(t, engine, taskID)
	original := engine.FirstPendingDecision(taskID)
	if original == nil {
		t.Fatal("no delegation decision")
	}
	payload := `{"status":"capability_required","confidence":0.3,"result":{},"required_capabilities":["browser"]}`
	if err := engine.FulfillStep(taskID, original.ID, payload); err != nil {
		t.Fatalf("FulfillStep: %v", err)
	}

	graph := engine.GetTaskGraph(taskID)
	if graph == nil || len(graph.PendingDecisions) != 1 {
		t.Fatalf("pending decisions = %#v, want one capability decision", graph)
	}
	dec := graph.PendingDecisions[0]
	advisory, _ := dec.Context["capability_advisory"].(map[string]any)
	discover, _ := advisory["discover"].(string)
	if !strings.Contains(discover, `scope="self"`) {
		t.Errorf("public discover advisory = %q, want self scope", discover)
	}
	if strings.Contains(discover, `scope="pool"`) {
		t.Errorf("public discover advisory exposes admin-only pool scope: %q", discover)
	}
	if invoke, _ := advisory["invoke"].(string); invoke != "" {
		t.Errorf("public host received admin-only proxy.call advisory: %q", invoke)
	}
	for _, option := range dec.Options {
		if strings.HasPrefix(option, "retry_with_skill:") || option == "rewrite_dag" {
			t.Errorf("public host received admin-only option %q", option)
		}
	}
	if step := graph.Steps["s1"]; step == nil || step.Status != schemas.StepBlocked {
		t.Fatalf("step = %#v, want non-OK blocked state", step)
	}
}

func decisionHasOption(options []string, want string) bool {
	for _, option := range options {
		if option == want {
			return true
		}
	}
	return false
}
