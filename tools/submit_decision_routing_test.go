package tools

import "testing"

// T1 regression: replan/create_role with non-empty output must NOT route
// to FulfillStep. Before the fix, the condition
//   (choice == "fulfill" || output != "") && choice != "rewrite_dag"
// evaluated true for replan/create_role with output, swallowing the
// structured payload into FulfillStep instead of reaching
// scheduler_status.go's replan/create_role cases.
func TestShouldRouteToFulfill_T1Regression(t *testing.T) {
	cases := []struct {
		name   string
		choice string
		output string
		want   bool // true = route to FulfillStep
	}{
		// T1: these MUST route to SubmitDecisionWithPayload (false)
		{"replan with payload", "replan", `{"replacement_steps":[]}`, false},
		{"create_role with payload", "create_role", `{"role_draft":{}}`, false},
		{"rewrite_dag with payload", "rewrite_dag", `{"deprecated_step_id":"s1"}`, false},
		{"replan without payload", "replan", "", false},
		{"create_role without payload", "create_role", "", false},

		// Legacy behavior preserved: fulfill + output routes to FulfillStep
		{"fulfill with output", "fulfill", `{"result":"ok"}`, true},
		{"fulfill without output", "fulfill", "", true}, // edge: fulfill always routes to fulfill path

		// Other choices with output: legacy behavior routes to FulfillStep
		{"skip with output", "skip", `{"reason":"done"}`, true},
		{"abort with output", "abort", `{"reason":"bad"}`, true},

		// Other choices without output: route to SubmitDecisionWithPayload
		{"skip without output", "skip", "", false},
		{"abort without output", "abort", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRouteToFulfill(tc.choice, tc.output)
			if got != tc.want {
				t.Fatalf("shouldRouteToFulfill(%q, %q) = %v, want %v", tc.choice, tc.output, got, tc.want)
			}
		})
	}
}

// T2 regression: public tier allows replan/create_role but rejects payload
// injection. Admin tier is unrestricted.
func TestValidateTierChoice_T2Regression(t *testing.T) {
	cases := []struct {
		name    string
		tier    string
		choice  string
		output  string
		wantErr bool
	}{
		// Public tier: basic choices allowed
		{"public skip", TierPublic, "skip", "", false},
		{"public abort", TierPublic, "abort", "", false},
		{"public retry", TierPublic, "retry", "", false},
		{"public fulfill", TierPublic, "fulfill", `{"r":"ok"}`, false},

		// T2: public replan allowed without payload
		{"public replan no payload", TierPublic, "replan", "", false},
		// T2: public replan REJECTED with payload (injection risk)
		{"public replan with payload", TierPublic, "replan", `{"replacement_steps":[]}`, true},

		// T2: public create_role allowed without payload
		{"public create_role no payload", TierPublic, "create_role", "", false},
		// T2: public create_role REJECTED with payload (injection risk)
		{"public create_role with payload", TierPublic, "create_role", `{"role_draft":{}}`, true},

		// Public tier: admin-only choices rejected
		{"public rewrite_dag", TierPublic, "rewrite_dag", "", true},
		{"public unknown choice", TierPublic, "bogus", "", true},

		// Admin tier: everything allowed
		{"admin replan with payload", "admin", "replan", `{"replacement_steps":[]}`, false},
		{"admin create_role with payload", "admin", "create_role", `{"role_draft":{}}`, false},
		{"admin rewrite_dag", "admin", "rewrite_dag", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTierChoice(tc.tier, tc.choice, tc.output)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for tier=%q choice=%q output=%q", tc.tier, tc.choice, tc.output)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for tier=%q choice=%q output=%q: %v", tc.tier, tc.choice, tc.output, err)
			}
		})
	}
}

