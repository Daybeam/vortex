package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// TestCanonicalToolArgs_StripsControlArgs locks the contract that engine
// control arguments are invisible to loop detection while still readable by
// the reflection gate.
func TestCanonicalToolArgs_StripsControlArgs(t *testing.T) {
	in := map[string]any{
		"query":        "rent",
		"_reason":      "retrying because the page was stale",
		"_decision_id": "dec-1",
		"page_limit":   float64(20),
	}
	out := canonicalToolArgs(in)

	if _, ok := out["_reason"]; ok {
		t.Fatal("_reason must be stripped")
	}
	if _, ok := out["_decision_id"]; ok {
		t.Fatal("_decision_id must be stripped")
	}
	if out["query"] != "rent" {
		t.Fatalf("query preserved? got %v", out["query"])
	}
	// The input must not be mutated: #11 reads _reason after the key is built.
	if in["_reason"] == nil {
		t.Fatal("canonicalToolArgs must not mutate its input")
	}
	if len(in) != 4 {
		t.Fatalf("input map size changed: %d", len(in))
	}
}

// TestCanonicalToolArgs_NoControlArgsNoCopy verifies the fast path returns the
// original map untouched, so the common case allocates nothing.
func TestCanonicalToolArgs_NoControlArgsNoCopy(t *testing.T) {
	in := map[string]any{"query": "rent"}
	out := canonicalToolArgs(in)
	if len(out) != 1 || out["query"] != "rent" {
		t.Fatalf("unexpected result: %v", out)
	}
}

// TestToolCallKey_ReasonDoesNotChangeKey is the regression guard for the
// bypass found on 4fab96f_3: a model that rephrases _reason used to mint a
// fresh key for an otherwise identical call, defeating A1, #11 and every
// Sieve check at once.
func TestToolCallKey_ReasonDoesNotChangeKey(t *testing.T) {
	base := map[string]any{"access_token": "tok", "page_limit": float64(20)}
	withReason := map[string]any{
		"access_token": "tok",
		"page_limit":   float64(20),
		"_reason":      "the previous read was truncated",
	}
	if a, b := toolCallKey("venmo__show_sent_payment_requests", base),
		toolCallKey("venmo__show_sent_payment_requests", withReason); a != b {
		t.Fatalf("_reason changed the dedup key:\n  without=%s\n  with   =%s", a, b)
	}

	// Genuinely different arguments must still produce a different key,
	// otherwise the dedup would collapse legitimate distinct calls.
	other := map[string]any{"access_token": "tok", "page_limit": float64(50)}
	if a, c := toolCallKey("venmo__show_sent_payment_requests", base),
		toolCallKey("venmo__show_sent_payment_requests", other); a == c {
		t.Fatal("different arguments must not collide")
	}
}

// TestSieve_ReasonCannotResetConsecutiveCounter proves the enforcement side is
// reason-blind: feeding the same call repeatedly while varying _reason still
// trips the interception, because the hash ignores control arguments.
func TestSieve_ReasonCannotResetConsecutiveCounter(t *testing.T) {
	s := NewSieve(100)
	taskID, stepID := "task-reason-bypass", "step-1"

	reasons := []string{"stale page", "retry", "different wording", "yet another"}
	interceptedAt := 0
	for i, r := range reasons {
		calls := []store.ToolInteraction{{
			ToolName: "venmo__show_sent_payment_requests",
			Arguments: map[string]any{
				"access_token": "tok",
				"page_limit":   float64(20),
				"_reason":      r,
			},
		}}
		valid, reason := s.InspectToolCalls(taskID, stepID, calls)
		if !valid {
			interceptedAt = i + 1
			if reason != "InfiniteLoopDetected" {
				t.Fatalf("unexpected reason %q", reason)
			}
			break
		}
	}

	// Default ToolRepetitionThreshold is 3: the 4th identical call is the first
	// one whose history already holds three matches.
	if interceptedAt != 4 {
		t.Fatalf("expected interception on call 4, got %d — a rephrased _reason still resets the counter", interceptedAt)
	}
}

// TestNoteToolResult_CountsAnswersNotCalls is the regression guard for the
// false positive observed in passk_20261021_074251 trial 2: venmo__login with
// identical username/password is a legitimate retry because each call returns a
// new token. The streak must only grow while the answer stays byte-identical.
func TestNoteToolResult_CountsAnswersNotCalls(t *testing.T) {
	state := &toolExecState{
		resultStreak:   map[string]int{},
		resultLastHash: map[string]uint64{},
	}
	key := "venmo__login:abc"

	// A token refresh returns a different payload each time → must never build
	// a streak, however many times it is called.
	for i := 0; i < 10; i++ {
		noteToolResult(state, key, "token-"+string(rune('a'+i)))
		if state.resultStreak[key] != 1 {
			t.Fatalf("call %d: changing answer must keep streak at 1, got %d", i+1, state.resultStreak[key])
		}
	}

	// A genuinely stuck read returns the same payload → streak must grow and
	// eventually trip the cap.
	for i := 1; i <= defaultDedupHardCap; i++ {
		noteToolResult(state, key, "identical-body")
		if state.resultStreak[key] != i {
			t.Fatalf("call %d: identical answer must streak, got %d", i, state.resultStreak[key])
		}
	}
	if state.resultStreak[key] < defaultDedupHardCap {
		t.Fatalf("streak %d should have reached the cap %d", state.resultStreak[key], defaultDedupHardCap)
	}

	// Switching tools must not share a streak.
	noteToolResult(state, "other_tool:def", "identical-body")
	if state.resultStreak["other_tool:def"] != 1 {
		t.Fatal("different call keys must not share a streak")
	}

	// Empty payloads are ignored (a tool that returns nothing tells us nothing).
	before := state.resultStreak[key]
	noteToolResult(state, key, "")
	if state.resultStreak[key] != before {
		t.Fatal("empty result must not affect the streak")
	}
}

// TestDedupHardCap_Resolution checks the precedence documented on
// Spawner.dedupHardCap: deployment setting, then built-in default.
func TestDedupHardCap_Resolution(t *testing.T) {
	// Nil registry → default.
	if got := (&Spawner{}).dedupHardCap(); got != defaultDedupHardCap {
		t.Fatalf("nil registry: want %d, got %d", defaultDedupHardCap, got)
	}
	if defaultDedupHardCap != 5 {
		t.Fatalf("defaultDedupHardCap changed to %d — update the rationale in docs/54 §5.18.9", defaultDedupHardCap)
	}

	// Explicit setting wins.
	reg := &config.Registry{System: config.SystemSettings{DedupHardCap: 3}}
	if got := (&Spawner{registry: reg}).dedupHardCap(); got != 3 {
		t.Fatalf("configured cap: want 3, got %d", got)
	}

	// Zero/unset falls back to the default rather than blocking every repeat.
	reg0 := &config.Registry{System: config.SystemSettings{DedupHardCap: 0}}
	if got := (&Spawner{registry: reg0}).dedupHardCap(); got != defaultDedupHardCap {
		t.Fatalf("unset cap: want %d, got %d", defaultDedupHardCap, got)
	}
}
