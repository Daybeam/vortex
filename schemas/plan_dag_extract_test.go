package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// observedThoughtOnly is the real answer that broke planner_first on
// 2026-10-19 (task_96ac531e, gemini-3-flash, AppWorld 4fab96f_3). It contains
// no JSON whatsoever, so no extractor can save it — ParsePlanDAG must reject it
// and the caller must retry. This fixture exists so a future "tolerant" parser
// change cannot quietly start inventing a plan out of prose.
const observedThoughtOnly = `<thought>
The user wants to send reminders for Venmo payment requests sent to roommates that are pending for 50+ days.
I need to:
1. Identify who the roommates are (using supervisor tools and potentially Venmo transaction history).
2. Login to Venmo.
3. Get sent payment requests.
4. Filter by recipient (roommate), status (pending), and date (>= 50 days).
5. Send reminders for matching requests.

I'll use a single ` + "`appworld_agent`" + ` step to accomplish this as it's a cohesive task and the agent can handle the logic.

Steps:
1. ` + "`remind_roommates_pending_requests`" + `: Identify roommates, fetch requests, filter, and send reminders`

func TestFindBalancedObject_ThoughtBlockThenPlan(t *testing.T) {
	raw := "<thought>\nLet me decompose this into read and write.\n</thought>\n\n" +
		`{"steps":[{"id":"s1","role_id":"appworld_agent","task":"read","depends_on":[],"allowed_tools":[]}]}`

	got, ok := findBalancedObject(raw)
	if !ok {
		t.Fatal("expected to find a balanced JSON object after a thought block")
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("extracted span is not valid JSON: %q", got)
	}
	if !strings.Contains(got, `"steps"`) {
		t.Fatalf("extracted wrong span: %q", got)
	}
}

func TestFindBalancedObject_BareJSON(t *testing.T) {
	raw := `{"steps":[]}`
	got, ok := findBalancedObject(raw)
	if !ok || got != raw {
		t.Fatalf("bare JSON must be returned unchanged: got %q ok=%v", got, ok)
	}
}

func TestFindBalancedObject_IgnoresBracesInsideStrings(t *testing.T) {
	// The task text itself contains braces. A naive "first { to last }" scan
	// would still work here, but a naive depth counter that ignores string
	// literals would close early and emit an invalid fragment.
	raw := `<thought>use the { placeholder } form</thought>` +
		`{"steps":[{"id":"s1","role_id":"r","task":"write {\"a\":1} to the file","depends_on":[],"allowed_tools":[]}]}`

	got, ok := findBalancedObject(raw)
	if !ok {
		t.Fatal("expected to find the plan despite braces in prose and in a task string")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("extracted span is not valid JSON: %v (%q)", err, got)
	}
	steps, _ := m["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
}

func TestFindBalancedObject_SkipsNonJSONBracePair(t *testing.T) {
	// Prose uses braces first; the scan must reject that pair and keep going
	// instead of returning it and failing the whole answer.
	raw := `Consider the set { a, b } and then output:` +
		`{"steps":[{"id":"s1","role_id":"r","task":"t","depends_on":[],"allowed_tools":[]}]}`

	got, ok := findBalancedObject(raw)
	if !ok {
		t.Fatal("expected the scan to skip the prose brace pair and find the plan")
	}
	if !strings.Contains(got, `"role_id"`) {
		t.Fatalf("returned the prose brace pair instead of the plan: %q", got)
	}
}

func TestFindBalancedObject_NoJSON(t *testing.T) {
	if got, ok := findBalancedObject(observedThoughtOnly); ok {
		t.Fatalf("thought-only output has no JSON; must not fabricate one. got %q", got)
	}
}

func TestFindBalancedObject_TruncatedIsNotBalanced(t *testing.T) {
	// A truncated answer has no closing brace, so the scan finds nothing and
	// jsonrepair's TrimIncomplete path stays the only recourse.
	raw := `{"steps":[{"id":"s1","role_id":"appworld_agent"`
	if got, ok := findBalancedObject(raw); ok {
		t.Fatalf("unbalanced input must not match: %q", got)
	}
}

func TestExtractJSONObject_ThoughtThenFencedPlan(t *testing.T) {
	// Prose, then a fenced block: the fence-stripper already handles this, but
	// the balanced scan must not regress it.
	raw := "<thought>I will split read from write.</thought>\n\n```json\n" +
		`{"steps":[{"id":"s1","role_id":"appworld_agent","task":"read","depends_on":[],"allowed_tools":[]}]}` +
		"\n```"

	got, err := extractJSONObject(raw)
	if err != nil {
		t.Fatalf("extractJSONObject failed: %v", err)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("not valid JSON: %q", got)
	}
}

func TestExtractJSONObject_RejectsThoughtOnly(t *testing.T) {
	if _, err := extractJSONObject(observedThoughtOnly); err == nil {
		t.Fatal("thought-only output must be rejected; a retry is the correct response")
	}
}
