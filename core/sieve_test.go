package core

import (
	"testing"

	"github.com/daybeam/vortex/store"
)

// Feature: Sieve Guardian
// Scenario: Repetition detection — when output repetition rate exceeds threshold,
//          the call is intercepted
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Sieve Guardian
// -----------------------------------------------------------------------------
func TestSieve_BlocksWhenRepetitionExceedsThreshold(t *testing.T) {
	s := NewSieve(10)
	s.WindowSize = 2
	s.RepetitionThreshold = 0.9

	text := "line 1\nline 2\nline 1\nline 2"
	valid, reason := s.Inspect(text, "")
	if valid {
		t.Errorf("Expected invalid due to repetition, got valid")
	}
	if reason != "repetition detected: output entered a logic loop" {
		t.Errorf("Expected repetition reason, got %q", reason)
	}
}

// Regression: Sieve.InspectToolCalls missed sparse interleaved repetition.
// The consecutive-count check breaks on first non-match, and the window
// check only scans the last threshold*2 entries with a 50% density gate.
// Both miss the pattern A,B,C,A,B,C,A,... where no 6-window ever has 3+
// of the same tool (max 2/6 = 33% < 50% density). The eval observed
// get_reservation_details called 35× across turns without interception.
// Fix: total-occurrence check at threshold*2 (sieve.go InspectToolCalls).
// Bug: eval-kit/docs/24-doc-map-and-remaining-work.md #2.
func TestSieve_InspectToolCalls_SparseInterleavedRepetition(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3 // total threshold = 3*2 = 6

	taskID := "task-sparse-loop"
	toolA := store.ToolInteraction{ToolName: "get_reservation_details", Arguments: map[string]any{"id": "X1"}}
	toolB := store.ToolInteraction{ToolName: "get_user_details", Arguments: map[string]any{"id": "U1"}}
	toolC := store.ToolInteraction{ToolName: "get_flight_status", Arguments: map[string]any{"id": "F1"}}

	// A,B,C,A,B,C,A,B,C,A,B,C,A,B,C,A,B,C,A — 7×A, 6×B, 6×C (19 calls).
	// No 6-window has ≥3 of any tool (max 2, density 33% < 50%),
	// and no tool ever appears consecutively. Old logic never fires.
	// New total-count check fires on the 7th A (totalCount=6 ≥ 6).
	calls := []store.ToolInteraction{
		toolA, toolB, toolC, toolA, toolB, toolC, toolA, toolB, toolC,
		toolA, toolB, toolC, toolA, toolB, toolC, toolA, toolB, toolC, toolA,
	}

	blocked := false
	for _, c := range calls {
		ok, _ := s.InspectToolCalls(taskID, []store.ToolInteraction{c})
		if !ok {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Errorf("Expected sparse interleaved repetition (7× same tool+args) to be intercepted")
	}
}

// Regression companion: verify the fix does NOT false-positive on
// legitimate retry/polling (same tool+args called 5× interleaved with
// two other tools — under the total threshold of 6, and under the
// window density gate of 50%).
func TestSieve_InspectToolCalls_AllowsLegitimateRetry(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3 // total threshold = 6

	taskID := "task-legit-retry"
	toolA := store.ToolInteraction{ToolName: "get_status", Arguments: map[string]any{"job": "J1"}}
	toolB := store.ToolInteraction{ToolName: "do_work", Arguments: map[string]any{"step": 1}}
	toolC := store.ToolInteraction{ToolName: "check_result", Arguments: map[string]any{"step": 1}}

	// A,B,C,A,B,C,A,B,C,A,B,C,A — 5×A, 4×B, 4×C (13 calls).
	// No 6-window has ≥3 of any tool (max 2, density 33% < 50%),
	// and totalCount of A peaks at 4 < 6. Not blocked by any check.
	calls := []store.ToolInteraction{
		toolA, toolB, toolC, toolA, toolB, toolC, toolA, toolB, toolC,
		toolA, toolB, toolC, toolA,
	}

	for _, c := range calls {
		ok, reason := s.InspectToolCalls(taskID, []store.ToolInteraction{c})
		if !ok {
			t.Fatalf("Legitimate retry (5× sparse interleaved) should not be blocked, got: %s", reason)
		}
	}
}

// Regression: force-terminate consequence replaced by cache replay.
// When a read tool is called with the same args, the Sieve should return
// the cached result instead of signaling InfiniteLoopDetected. A write tool
// should clear the cache so the next read executes fresh (write-then-read-back).
// Bug: eval-kit/docs/24-doc-map-and-remaining-work.md #2 — force-terminate
// caused 4B model to produce empty turns / transfer-to-human (pass rate 1/5→0/5).
func TestSieve_CacheReplay_ReturnsCachedResultForRepeatRead(t *testing.T) {
	s := NewSieve(10)
	taskID := "task-cache-replay"

	readArgs := map[string]any{"id": "X1"}
	s.CacheToolResult(taskID, "get_reservation_details", readArgs, "reservation X1: confirmed, seat 12A")

	cached, found := s.GetCachedResult(taskID, "get_reservation_details", readArgs)
	if !found {
		t.Fatal("Expected cached result for repeated read call")
	}
	if cached != "reservation X1: confirmed, seat 12A" {
		t.Errorf("Unexpected cached result: %q", cached)
	}
}

func TestSieve_CacheReplay_WriteClearsCache(t *testing.T) {
	s := NewSieve(10)
	taskID := "task-write-clears"

	readArgs := map[string]any{"id": "X1"}
	s.CacheToolResult(taskID, "get_reservation_details", readArgs, "reservation X1: confirmed")

	s.ClearResultCache(taskID)

	_, found := s.GetCachedResult(taskID, "get_reservation_details", readArgs)
	if found {
		t.Fatal("Cache should be cleared after write operation")
	}
}

func TestSieve_CacheReplay_DifferentArgsNotCached(t *testing.T) {
	s := NewSieve(10)
	taskID := "task-diff-args"

	s.CacheToolResult(taskID, "get_reservation_details", map[string]any{"id": "X1"}, "result X1")

	_, found := s.GetCachedResult(taskID, "get_reservation_details", map[string]any{"id": "X2"})
	if found {
		t.Fatal("Different args should not hit cache")
	}
}

func TestSieve_CacheReplay_ClearHistoryAlsoClearsCache(t *testing.T) {
	s := NewSieve(10)
	taskID := "task-clear-history"

	s.CacheToolResult(taskID, "get_status", map[string]any{"job": "J1"}, "running")

	s.ClearHistory(taskID)

	_, found := s.GetCachedResult(taskID, "get_status", map[string]any{"job": "J1"})
	if found {
		t.Fatal("ClearHistory should also clear result cache")
	}
}

func TestSieve_isReadTool(t *testing.T) {
	readTools := []string{"get_reservation_details", "get_flight_status", "list_users", "check_status", "query_orders", "search_flights", "read_file", "fetch_data", "is_valid", "has_permission"}
	for _, name := range readTools {
		if !isReadTool(name) {
			t.Errorf("Expected %q to be classified as read tool", name)
		}
	}
	writeTools := []string{"cancel_reservation", "book_flight", "update_booking", "create_order", "delete_user", "submit_form", "modify_record", "transfer_call", "rebook_flight", "reschedule_flight"}
	for _, name := range writeTools {
		if isReadTool(name) {
			t.Errorf("Expected %q to be classified as write tool", name)
		}
	}
}

// Gap-2: Poller tools (poll/progress/health/wait) return time-varying results
// with the same args. They must be exempt from loop detection and never cached.
// Bug: TOOL_LOOP_GUARDRAILS_PLAN.md Gap-2 — cache replay would return stale
// data for pollers; total-occurrence check would false-flag polling as a loop.
func TestSieve_isPollerTool(t *testing.T) {
	pollers := []string{"poll_job", "get_progress", "check_health", "wait_for_completion", "poll_result", "query_progress"}
	for _, name := range pollers {
		if !isPollerTool(name) {
			t.Errorf("Expected %q to be classified as poller", name)
		}
	}
	nonPollers := []string{"get_reservation_details", "cancel_reservation", "check_status", "get_flight_status"}
	for _, name := range nonPollers {
		if isPollerTool(name) {
			t.Errorf("Expected %q to NOT be classified as poller", name)
		}
	}
}

// Poller tools are exempt from all repetition checks in InspectToolCalls.
// Calling poll_job 10× with the same args should NOT trigger InfiniteLoopDetected.
func TestSieve_PollerExemptFromLoopDetection(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3

	taskID := "task-poller"
	pollCall := store.ToolInteraction{ToolName: "poll_job", Arguments: map[string]any{"job": "J1"}}

	for i := 0; i < 10; i++ {
		ok, reason := s.InspectToolCalls(taskID, []store.ToolInteraction{pollCall})
		if !ok {
			t.Fatalf("Poller tool should be exempt from loop detection, got: %s (call %d)", reason, i+1)
		}
	}
}

// Non-poller tools with the same pattern SHOULD still be detected.
func TestSieve_NonPollerStillDetected(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3

	taskID := "task-non-poller"
	readCall := store.ToolInteraction{ToolName: "get_data", Arguments: map[string]any{"id": "X1"}}

	blocked := false
	for i := 0; i < 10; i++ {
		ok, _ := s.InspectToolCalls(taskID, []store.ToolInteraction{readCall})
		if !ok {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("Non-poller tool should still be detected for excessive repetition")
	}
}

// Feature: Content-based result repetition detection
// Scenario: Different tool calls return identical result content (e.g., entity
//          ID variants like "sophia" vs "SOPHIA" returning the same user data).
//          The call-level hash check misses this because args differ. The
//          result-level check catches it by comparing result content hashes.
// Inspired by: OpenHands StuckDetector content-based event equality.
// See: docs/HARNESS_LOOP_GUARD_RESEARCH.md §7.
func TestSieve_CheckResultRepetition_SameResultDifferentCalls(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3

	taskID := "task-result-rep"
	result := `{"name":"Sophia Martin","email":"sophia@example.com","id":"4574"}`

	for i := 1; i <= s.ToolRepetitionThreshold+1; i++ {
		triggered := s.CheckResultRepetition(taskID, result)
		if i <= s.ToolRepetitionThreshold && triggered {
			t.Fatalf("Call %d: should not trigger yet (need %d prior occurrences, have %d)", i, s.ToolRepetitionThreshold, i-1)
		}
		if i == s.ToolRepetitionThreshold+1 && !triggered {
			t.Fatalf("Call %d: should trigger (threshold reached)", i)
		}
	}
}

func TestSieve_CheckResultRepetition_DifferentResultsNoTrigger(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3

	taskID := "task-diff-results"

	results := []string{
		`{"name":"Sophia Martin","email":"sophia@example.com"}`,
		`{"name":"John Doe","email":"john@example.com"}`,
		`{"name":"Jane Smith","email":"jane@example.com"}`,
		`{"name":"Bob Wilson","email":"bob@example.com"}`,
	}

	for i, r := range results {
		if s.CheckResultRepetition(taskID, r) {
			t.Fatalf("Call %d with different result should not trigger repetition", i+1)
		}
	}
}

func TestSieve_CheckResultRepetition_ClearHistoryResets(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3

	taskID := "task-clear-test"
	result := `{"status":"ok","data":"same"}`

	s.CheckResultRepetition(taskID, result)
	s.CheckResultRepetition(taskID, result)

	s.ClearHistory(taskID)

	if s.CheckResultRepetition(taskID, result) {
		t.Fatal("After ClearHistory, first call should not trigger repetition")
	}
}
