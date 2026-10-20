package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestIsReadTool_NamespacedMCPTools pins the namespace handling that was missing
// until 2026-10-19.
//
// Before the fix, isReadTool compared the read verbs against the FULL tool name.
// Every MCP tool is named "<server>__<method>", so "venmo__show_transactions"
// was tested against the prefix "venmo" — no match — and classified as a WRITE.
// For AppWorld that meant all ~60 tools across 10 apps were writes: the read
// cache never stored anything, and result-repetition detection was gated off.
func TestIsReadTool_NamespacedMCPTools(t *testing.T) {
	reads := []string{
		"venmo__show_transactions",
		"venmo__show_sent_payment_requests",
		"supervisor__show_addresses",
		"spotify__show_playlist_library",
		"gmail__search_messages",
		"todoist__get_task",
		"file_system__list_files",
		"splitwise__fetch_group",
	}
	for _, name := range reads {
		if !isReadTool(name) {
			t.Errorf("%q must be a read tool; classifying namespaced reads as writes silently disables the read cache and the repetition guard", name)
		}
	}

	writes := []string{
		"venmo__remind_payment_request",
		"venmo__create_payment_request",
		"venmo__delete_payment_card",
		"supervisor__complete_task",
		"gmail__send_email",
		"todoist__create_task",
	}
	for _, name := range writes {
		if isReadTool(name) {
			t.Errorf("%q must stay a write tool — treating it as a read would let a mutation look repeatable", name)
		}
	}

	// Unprefixed names must behave exactly as before the change.
	if !isReadTool("get_flight_status") {
		t.Error("unprefixed read regressed")
	}
	if isReadTool("cancel_reservation") {
		t.Error("unprefixed write regressed")
	}
}

// TestObserveResult_ArgumentProofRepetition is the core of the fix.
//
// InspectToolCalls hashes tool + arguments, so permuting arguments mints a new
// hash and the guard never fires. ObserveResult hashes the RESULT, which is what
// actually repeats when a model re-asks the same question in different words.
func TestObserveResult_ArgumentProofRepetition(t *testing.T) {
	s := NewSieve(100)
	const payload = `{"pending":[{"id":1,"amount":450},{"id":2,"amount":120}]}`

	// Three calls, same payload, as if the model had permuted `query`.
	// The sieve never sees the arguments here — that is the point.
	for want := 1; want <= 3; want++ {
		if got := s.ObserveResult("task-1", "step-1", "venmo__show_transactions", payload); got != want {
			t.Fatalf("occurrence %d: got %d", want, got)
		}
	}

	// A different payload for the same tool is a different observation.
	if got := s.ObserveResult("task-1", "step-1", "venmo__show_transactions", payload+"\n "); got != 1 {
		t.Errorf("changed payload counted as a repeat (got %d)", got)
	}

	// Different tool, same payload: not a repeat of that tool.
	if got := s.ObserveResult("task-1", "step-1", "venmo__show_sent_payment_requests", payload); got != 1 {
		t.Errorf("cross-tool payload counted as a repeat (got %d)", got)
	}

	// Different step, same payload: DAG steps may legitimately re-read.
	if got := s.ObserveResult("task-1", "step-2", "venmo__show_transactions", payload); got != 1 {
		t.Errorf("cross-step payload counted as a repeat (got %d); that misfires on gather→format DAGs", got)
	}

	// ClearHistory wipes the task's counters, including step-scoped ones.
	s.ClearHistory("task-1")
	if got := s.ObserveResult("task-1", "step-1", "venmo__show_transactions", payload); got != 1 {
		t.Errorf("ClearHistory left stale counters (got %d)", got)
	}
}

// newTestSpawner builds the minimum Spawner that steerRepeatedResult touches.
func newTestSpawner(t *testing.T) *Spawner {
	t.Helper()
	reg := &config.Registry{
		Roles: map[string]*config.Role{"worker": {ID: "worker", BaseCapability: "text"}},
		MCPs:  map[string]*config.MCPDef{},
	}
	return &Spawner{
		registry: reg,
		logger:   mustNewLogger(t, t.TempDir(), &reg.System),
		sieve:    newSieveFromConfig(reg.System),
	}
}

// TestSteerRepeatedResult_TwoStageEscalation pins what the model actually reads.
//
// The measured failure (4fab96f_3, 2026-10-19): 61 calls to show_transactions and
// 30 to show_sent_payment_requests, two distinct payloads, 91 of 102 calls pure
// re-reads, then MAX_TURNS_EXHAUSTED. Each duplicate also cost 7.4K characters of
// context, so by turn 40 the window was full of copies of two JSON blobs.
func TestSteerRepeatedResult_TwoStageEscalation(t *testing.T) {
	s := newTestSpawner(t)
	const tool = "venmo__show_transactions"
	const payload = `{"transactions":[{"id":1,"note":"Rent"}]}`

	// First sighting: untouched.
	if got := s.steerRepeatedResult("t1", "s1", tool, payload); got != payload {
		t.Fatalf("first result was modified:\n%s", got)
	}

	// Second: a nudge, payload still present so the model can act on it.
	second := s.steerRepeatedResult("t1", "s1", tool, payload)
	if !strings.HasPrefix(second, "[RESULT REPETITION]") {
		t.Errorf("second identical result got no steer:\n%s", second)
	}
	if !strings.Contains(second, payload) {
		t.Error("steer dropped the payload; the model would lose the only data it has")
	}
	if !strings.Contains(second, tool) {
		t.Error("steer does not name the tool, so it is not actionable")
	}

	// Third: still a steer (same stage).
	if got := s.steerRepeatedResult("t1", "s1", tool, payload); !strings.HasPrefix(got, "[RESULT REPETITION]") {
		t.Errorf("third identical result should still steer, got:\n%s", got)
	}

	// Fourth: withhold. The payload is already in context; a fourth copy is pure
	// displacement.
	fourth := s.steerRepeatedResult("t1", "s1", tool, payload)
	if strings.Contains(fourth, payload) {
		t.Errorf("fourth identical result still carried the %d-char payload", len(payload))
	}
	if !strings.Contains(fourth, "withheld") || !strings.Contains(fourth, tool) {
		t.Errorf("withhold message is not actionable:\n%s", fourth)
	}
	// The point of withholding is context relief, so the stub must be short in
	// absolute terms — not merely shorter than this toy payload. The payloads
	// that motivated the change were 7.4-7.6K characters each.
	if len(fourth) > 600 {
		t.Errorf("withhold stub is too long to relieve context pressure (%d chars)", len(fourth))
	}
}

// TestSteerRepeatedResult_Exemptions makes sure the guard does not reach tools
// where repetition is legitimate or meaningless.
func TestSteerRepeatedResult_Exemptions(t *testing.T) {
	s := newTestSpawner(t)
	const payload = "same"

	// Write tools: a write returning identical text is not a stuck read.
	for i := 0; i < 6; i++ {
		if got := s.steerRepeatedResult("t2", "s1", "venmo__remind_payment_request", payload); got != payload {
			t.Fatalf("write tool was steered on iteration %d", i)
		}
	}

	// Pollers: identical payloads are expected while waiting.
	for i := 0; i < 6; i++ {
		if got := s.steerRepeatedResult("t2", "s1", "job_wait_for_progress", payload); got != payload {
			t.Fatalf("poller was steered on iteration %d", i)
		}
	}

	// Nil sieve and empty payload must not panic or alter anything.
	empty := &Spawner{}
	if got := empty.steerRepeatedResult("t2", "s1", "venmo__show_transactions", payload); got != payload {
		t.Error("nil sieve changed the payload")
	}
	if got := s.steerRepeatedResult("t2", "s1", "venmo__show_transactions", ""); got != "" {
		t.Error("empty payload must pass through untouched")
	}
}
