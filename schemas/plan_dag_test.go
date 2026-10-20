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

// TestParsePlanDAG_UnwrapsExecutorEnvelope reproduces the exact shape the
// planner returned on AppWorld 4fab96f_3 (2026-10-19): a correct plan naming
// phone__get_current_date_and_time, wrapped in the executor's answer contract.
// It was rejected as "plan contains no steps" twice, the pipeline fell back to a
// single unplanned step, and the run could not get the current date.
func TestParsePlanDAG_UnwrapsExecutorEnvelope(t *testing.T) {
	raw := `{"status":"ok","confidence":1.0,"result":{"steps":[
	  {"id":"plan_dates","role_id":"appworld_agent","task":"Get today's date",
	   "allowed_tools":["phone__login"]},
	  {"id":"send_reminders","role_id":"appworld_agent","task":"Remind overdue",
	   "depends_on":["plan_dates"],
	   "allowed_tools":["venmo__remind_payment_request"]}
	]}}`

	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("executor envelope should be unwrapped, got: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	if steps[0].ID != "plan_dates" || len(steps[1].DependsOn) != 1 {
		t.Fatalf("unwrapped plan lost structure: %+v", steps)
	}
}

// TestParsePlanDAG_UnwrapsPlanKey covers the other wrapper we saw: the plan
// sitting under "plan" rather than "result".
func TestParsePlanDAG_UnwrapsPlanKey(t *testing.T) {
	raw := `{"plan":{"steps":[{"id":"s1","role_id":"appworld_agent","task":"do it"}]}}`
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("plan envelope should be unwrapped, got: %v", err)
	}
	if len(steps) != 1 || steps[0].ID != "s1" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

// TestParsePlanDAG_UnwrapsJSONStringEnvelope covers the doubly-encoded form,
// where the plan arrived as a JSON string inside "result".
func TestParsePlanDAG_UnwrapsJSONStringEnvelope(t *testing.T) {
	raw := `{"status":"ok","result":"{\"steps\":[{\"id\":\"s1\",\"role_id\":\"appworld_agent\",\"task\":\"do it\"}]}"}`
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("string-encoded plan should be unwrapped, got: %v", err)
	}
	if len(steps) != 1 || steps[0].ID != "s1" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

// TestParsePlanDAG_NoStepsNamesTheShape: the old rejection said only "plan
// contains no steps", which does not distinguish an empty plan from a
// mis-shaped envelope. The detail now names the top-level keys.
func TestParsePlanDAG_NoStepsNamesTheShape(t *testing.T) {
	_, err := ParsePlanDAG(`{"thought":"I will do it","answer":"done"}`, testPlanOptions())
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no steps") {
		t.Fatalf("expected no-steps reason, got: %v", msg)
	}
	for _, want := range []string{"answer", "thought"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("rejection should name top-level key %q so the shape is diagnosable; got: %v", want, msg)
		}
	}
}

// TestParsePlanDAG_SoleRoleFillsMissingRoleID: with exactly one legal role,
// omitting role_id is noise, not a decision. Rejecting it costs a full planner
// retry — observed on 4fab96f_3 as "step missing role_id (send_reminders)".
func TestParsePlanDAG_SoleRoleFillsMissingRoleID(t *testing.T) {
	raw := `{"steps":[{"id":"send_reminders","task":"Remind overdue"}]}`
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("sole-role default should apply, got: %v", err)
	}
	if steps[0].RoleID != "appworld_agent" {
		t.Fatalf("role_id not defaulted: %q", steps[0].RoleID)
	}
}

// TestParsePlanDAG_MissingRoleIDStillRejectedWhenAmbiguous is the counterweight:
// two legal roles means the omission is a real choice the model failed to make.
func TestParsePlanDAG_MissingRoleIDStillRejectedWhenAmbiguous(t *testing.T) {
	opt := testPlanOptions()
	opt.AllowedRoles = map[string]bool{"appworld_agent": true, "auditor": true}
	_, err := ParsePlanDAG(`{"steps":[{"id":"s1","task":"do it"}]}`, opt)
	if err == nil || !strings.Contains(err.Error(), "missing role_id") {
		t.Fatalf("ambiguous omission must still be rejected, got: %v", err)
	}
}
