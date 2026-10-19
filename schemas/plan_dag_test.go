package schemas

import (
	"strings"
	"testing"
)

func testPlanOptions() PlanParseOptions {
	return PlanParseOptions{
		AllowedRoles: map[string]bool{"appworld_agent": true},
		ToolCatalog: map[string]map[string]bool{
			"appworld": {
				"venmo__login":                      true,
				"venmo__show_sent_payment_requests": true,
				"venmo__remind_payment_request":     true,
				"phone__login":                      true,
				"phone__search_contacts":            true,
				"supervisor__complete_task":         true,
			},
		},
		SingleMCP: "appworld",
	}
}

func TestParsePlanDAG_HappyPath(t *testing.T) {
	raw := `{
	  "steps": [
	    {"id":"s1","role_id":"appworld_agent","task":"Find roommate emails",
	     "allowed_tools":["phone__login","phone__search_contacts"]},
	    {"id":"s2","role_id":"appworld_agent","task":"List pending sent requests",
	     "allowed_tools":["venmo__login","venmo__show_sent_payment_requests"]},
	    {"id":"s3","role_id":"appworld_agent","task":"Remind overdue debtors",
	     "depends_on":["s1","s2"],
	     "allowed_tools":["venmo__remind_payment_request"]}
	  ]
	}`

	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(steps))
	}
	if steps[2].ID != "s3" || len(steps[2].DependsOn) != 2 {
		t.Fatalf("s3 deps not preserved: %+v", steps[2])
	}
	got := steps[0].AdditionalToolAllowlists["appworld"]
	if len(got) != 2 || got[0] != "phone__login" {
		t.Fatalf("allowlist not attached: %+v", steps[0].AdditionalToolAllowlists)
	}
	if steps[0].AdditionalMCPs[0] != "appworld" {
		t.Fatalf("mcp not attached: %+v", steps[0].AdditionalMCPs)
	}
}

func TestParsePlanDAG_MarkdownFenced(t *testing.T) {
	raw := "Here is the plan:\n\n```json\n" +
		`{"steps":[{"id":"s1","role_id":"appworld_agent","task":"do it","allowed_tools":["venmo__login"]}]}` +
		"\n```\n\nLet me know if you need changes."
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 || steps[0].ID != "s1" {
		t.Fatalf("bad parse: %+v", steps)
	}
}

func TestParsePlanDAG_BareArray(t *testing.T) {
	raw := `[{"id":"s1","role_id":"appworld_agent","task":"do it","allowed_tools":["venmo__login"]}]`
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
}

func TestParsePlanDAG_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			name:    "empty output",
			raw:     "   ",
			wantSub: "empty planner output",
		},
		{
			name:    "no steps",
			raw:     `{"steps":[]}`,
			wantSub: "no steps",
		},
		{
			name:    "duplicate id",
			raw:     `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"a"},{"id":"s1","role_id":"appworld_agent","task":"b"}]}`,
			wantSub: "duplicate step id",
		},
		{
			name:    "unknown role",
			raw:     `{"steps":[{"id":"s1","role_id":"coder","task":"a"}]}`,
			wantSub: "unknown role_id",
		},
		{
			name:    "unknown tool",
			raw:     `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"a","allowed_tools":["venmo__nope"]}]}`,
			wantSub: "unknown tool name",
		},
		{
			name:    "dangling dependency",
			raw:     `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"a","depends_on":["s9"]}]}`,
			wantSub: "unknown step",
		},
		{
			name:    "self dependency",
			raw:     `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"a","depends_on":["s1"]}]}`,
			wantSub: "depends on itself",
		},
		{
			name: "cycle",
			raw: `{"steps":[
				{"id":"s1","role_id":"appworld_agent","task":"a","depends_on":["s2"]},
				{"id":"s2","role_id":"appworld_agent","task":"b","depends_on":["s1"]}]}`,
			wantSub: "cyclic dependency",
		},
		{
			name:    "missing task",
			raw:     `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"  "}]}`,
			wantSub: "missing task",
		},
		{
			name:    "illegal id chars",
			raw:     `{"steps":[{"id":"s 1/2","role_id":"appworld_agent","task":"a"}]}`,
			wantSub: "illegal characters",
		},
	}

	opt := testPlanOptions()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlanDAG(tc.raw, opt)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

// T7 fix (2026-10-18): when the caller cannot supply a live tool catalog
// (cold engine), the plan is now ACCEPTED with unverified tools instead of
// rejected. The tools will be intersected with discovered AvailableTools at
// execution time (validator.go ResolveFinalMCPs). This allows planner_first
// to produce accepted plans on a cold engine.
func TestParsePlanDAG_RefusesUnvalidatedAllowlist(t *testing.T) {
	raw := `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"a","allowed_tools":["venmo__login"]}]}`

	opt := testPlanOptions()
	opt.ToolCatalog = nil

	steps, err := ParsePlanDAG(raw, opt)
	if err != nil {
		t.Fatalf("T7: cold engine should accept unverified allowlist, got error: %v", err)
	}
	// Tool should pass through as-is (unverified)
	tools := steps[0].AdditionalToolAllowlists["appworld"]
	if len(tools) != 1 || tools[0] != "venmo__login" {
		t.Errorf("unverified tool should pass through as-is, got %v", tools)
	}
}

// An empty allowlist (planner omitted allowed_tools) is allowed and means
// "no per-step restriction". The router then applies the bound-MCP binding.
func TestParsePlanDAG_NoAllowlistMeansUnrestricted(t *testing.T) {
	raw := `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"a"}]}`
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps[0].AdditionalToolAllowlists) != 0 {
		t.Fatalf("expected no allowlist, got %+v", steps[0].AdditionalToolAllowlists)
	}
}

func TestParsePlanDAG_StepBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"steps":[`)
	for i := 0; i < 5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"s`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`","role_id":"appworld_agent","task":"a"}`)
	}
	b.WriteString(`]}`)

	opt := testPlanOptions()
	opt.MaxSteps = 3
	_, err := ParsePlanDAG(b.String(), opt)
	if err == nil || !strings.Contains(err.Error(), "step budget") {
		t.Fatalf("expected step-budget rejection, got: %v", err)
	}
}
