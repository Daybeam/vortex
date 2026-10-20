package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// The step text and tool list below are verbatim from run s (AppWorld 4fab96f_3,
// 2026-10-19 19:36) — a plan that gathers everything and sends nothing. It
// passed every existing check: it parsed, the roles were valid, the tool names
// were real, and the step executed to completion. It scored 0.0.
func TestPlanCoverageGap_DetectsPreparationOnlyPlan(t *testing.T) {
	userTask := "Send a reminder for each of my sent Venmo payment requests that is still pending after 50 days."

	prep := []schemas.StepInput{{
		ID:      "identify",
		RoleID:  "appworld_agent",
		Task:    "Identify roommates via supervisor__show_addresses, then login to Venmo and save pending requests to venmo_data.json.",
		DependsOn: []string{},
		AdditionalToolAllowlists: map[string][]string{"appworld": {
			"venmo__show_sent_payment_requests", "supervisor__show_profile", "venmo__show_profile",
			"supervisor__show_addresses", "venmo__show_transactions", "venmo__show_account",
			"venmo__search_friends", "venmo__login",
		}},
	}}

	gap := planCoverageGap(userTask, prep)
	if gap == "" {
		t.Fatal("a preparation-only plan for an action request must be rejected; this exact plan produced reward 0.0")
	}
	if !strings.Contains(gap, "send") {
		t.Errorf("gap message should name the missing verb, got: %s", gap)
	}

	// The fix: one action step, and the plan passes.
	withAction := append([]schemas.StepInput(nil), prep...)
	withAction = append(withAction, schemas.StepInput{
		ID:        "remind",
		RoleID:    "appworld_agent",
		Task:      "For each pending request older than 50 days, call venmo__remind_payment_request.",
		DependsOn: []string{"identify"},
		AdditionalToolAllowlists: map[string][]string{"appworld": {
			"venmo__remind_payment_request", "supervisor__complete_task",
		}},
	})
	if gap := planCoverageGap(userTask, withAction); gap != "" {
		t.Errorf("plan with an action step was rejected: %s", gap)
	}
}

func TestPlanCoverageGap_ReadOnlyRequestNeedsNoAction(t *testing.T) {
	steps := []schemas.StepInput{{
		ID: "look", RoleID: "appworld_agent", Task: "List my Venmo transactions.",
		AdditionalToolAllowlists: map[string][]string{"appworld": {"venmo__show_transactions"}},
	}}
	if gap := planCoverageGap("Show me my Venmo transactions from last week.", steps); gap != "" {
		t.Errorf("a read-only request must not be forced to produce an action step: %s", gap)
	}
}

// Login is not the action. Run s's only non-read tool was venmo__login, so a
// naive "has any non-read tool" test would have passed that plan.
func TestPlanCoverageGap_LoginIsNotAnAction(t *testing.T) {
	for _, name := range []string{"venmo__login", "supervisor__authenticate", "gmail__signin", "app__get_token"} {
		if isActionTool(name) {
			t.Errorf("%q must not count as an action tool", name)
		}
	}
	for _, name := range []string{"venmo__remind_payment_request", "todoist__create_task", "amazon__cancel_order", "supervisor__complete_task"} {
		if !isActionTool(name) {
			t.Errorf("%q must count as an action tool", name)
		}
	}
}

// A planner that declares no tools gets the benefit of the doubt: rejecting on
// absence would punish a convention rather than a mistake, and would break every
// plan that relies on the role's full surface.
func TestPlanCoverageGap_UndeclaredToolsAreNotJudged(t *testing.T) {
	steps := []schemas.StepInput{{ID: "one", RoleID: "appworld_agent", Task: "Do the thing."}}
	if gap := planCoverageGap("Send a reminder for pending requests.", steps); gap != "" {
		t.Errorf("plan with no declared tools was judged: %s", gap)
	}
}

// Word boundaries: real request text is full of near-misses.
func TestRequestNeedsAction_WordBoundaries(t *testing.T) {
	cases := []struct {
		task string
		want bool
	}{
		{"Send a reminder for each pending request", true},
		{"Please mark the task as complete", true},
		{"Delete the duplicate note", true},
		{"Show me the requests I have not approved or denied", false}, // "denied" must not trip "deny"
		{"List my playlists and summarize them", false},               // "summarize" must not trip "sum"
		{"Which payments were removed last week?", false},             // "removed" != "remove": the trailing 'd' fails the boundary
	}
	for _, c := range cases {
		_, got := requestNeedsAction(c.task)
		if got != c.want {
			t.Errorf("requestNeedsAction(%q) = %v, want %v", c.task, got, c.want)
		}
	}
}
