package sieve

import (
	"testing"
)

// TestIsReadTool_NamespacedMCPTools pins the namespace handling that was missing
// until 2026-10-19.
//
// Before the fix, IsReadTool compared the read verbs against the FULL tool name.
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
		if !IsReadTool(name) {
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
		if IsReadTool(name) {
			t.Errorf("%q must stay a write tool — treating it as a read would let a mutation look repeatable", name)
		}
	}

	// Unprefixed names must behave exactly as before the change.
	if !IsReadTool("get_flight_status") {
		t.Error("unprefixed read regressed")
	}
	if IsReadTool("cancel_reservation") {
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

// The steer-stage tests that exercise Spawner.steerRepeatedResult live in
// core (see core/sieve_steer_test.go): they need the Spawner harness, which
// core/sieve cannot import.

