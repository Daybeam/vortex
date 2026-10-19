package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// C3 regression: Output hash dedup must be consecutive-only, include tool
// name + params in the hash, skip short results and all-error turns, and
// preserve partial output on stagnation failure.
//
// Bug: The previous cumulative resultHashCounts map triggered false positives
// for legitimate polling (same hash on non-consecutive turns). Short results
// like get_current_date ("2026-10-18") and pure errors were also counted,
// double-penalizing tools already tracked by toolFailCount. On failure,
// failedOutput discarded all accumulated trace/work.

func TestComputeTurnHash_ShortResultsNotTracked(t *testing.T) {
	// Short results like dates, "OK", booleans should not participate in
	// stagnation detection — they legitimately repeat.
	calls := []schemas.ToolCall{{Name: "get_current_date", Arguments: map[string]any{}}}
	results := []string{"2026-10-18"}

	_, shouldTrack := computeTurnHash(calls, results)
	if shouldTrack {
		t.Errorf("short result (%d bytes < %d) should not be tracked, got shouldTrack=true",
			len(strings.Join(results, "\n")), MinResultBytesForDedup)
	}
}

func TestComputeTurnHash_AllErrorsNotTracked(t *testing.T) {
	// All-error turns are already tracked by toolFailCount; counting them
	// here would double-penalize.
	calls := []schemas.ToolCall{{Name: "search", Arguments: map[string]any{"q": "test"}}}
	results := []string{
		"### [TOOL RESULT] search\n\n**Execution Status**\n❌ ERROR | Source: mcp:search\n\n**Content**\nconnection refused",
		"### [TOOL RESULT] search\n\n**Execution Status**\n❌ FAILED | Source: mcp:search\n\n**Content**\ntimeout",
	}

	_, shouldTrack := computeTurnHash(calls, results)
	if shouldTrack {
		t.Errorf("all-error turn should not be tracked (already handled by toolFailCount), got shouldTrack=true")
	}
}

func TestComputeTurnHash_MixedErrorAndSuccessTracked(t *testing.T) {
	// If at least one result is not an error, the turn should be tracked.
	calls := []schemas.ToolCall{
		{Name: "search", Arguments: map[string]any{"q": "test"}},
		{Name: "read_file", Arguments: map[string]any{"path": "/tmp/data.json"}},
	}
	results := []string{
		"### [TOOL RESULT] search\n\n**Execution Status**\n❌ ERROR | Source: mcp:search\n\n**Content**\nnot found",
		strings.Repeat("### [TOOL RESULT] read_file\n\n**Execution Status**\n✅ SUCCESS | Source: mcp:read_file\n\n**Content**\n", 1) + strings.Repeat("data line\n", 20),
	}

	_, shouldTrack := computeTurnHash(calls, results)
	if !shouldTrack {
		t.Errorf("mixed error+success turn should be tracked, got shouldTrack=false")
	}
}

func TestComputeTurnHash_DifferentCallsDifferentHash(t *testing.T) {
	// Same results but different tool calls should produce different hashes.
	// This prevents false positives when two different tools return the same
	// short output (e.g., "OK").
	longResult := strings.Repeat("substantial data content\n", 10)
	calls1 := []schemas.ToolCall{{Name: "tool_a", Arguments: map[string]any{"x": 1}}}
	calls2 := []schemas.ToolCall{{Name: "tool_b", Arguments: map[string]any{"x": 2}}}
	results := []string{longResult}

	hash1, _ := computeTurnHash(calls1, results)
	hash2, _ := computeTurnHash(calls2, results)
	if hash1 == hash2 {
		t.Errorf("different tool calls should produce different hashes: hash1=%d hash2=%d", hash1, hash2)
	}
}

func TestComputeTurnHash_SameCallsSameResultsSameHash(t *testing.T) {
	// Identical calls + identical results should produce the same hash.
	// This is the actual stuck pattern we want to detect.
	longResult := strings.Repeat("identical data\n", 10)
	calls := []schemas.ToolCall{{Name: "fetch_data", Arguments: map[string]any{"id": "abc"}}}
	results := []string{longResult}

	hash1, _ := computeTurnHash(calls, results)
	hash2, _ := computeTurnHash(calls, results)
	if hash1 != hash2 {
		t.Errorf("identical calls+results should produce same hash: hash1=%d hash2=%d", hash1, hash2)
	}
}

func TestComputeTurnHash_DifferentArgsDifferentHash(t *testing.T) {
	// Same tool name but different arguments should produce different hashes.
	// The model making different calls is making progress, not stuck.
	longResult := strings.Repeat("result data\n", 10)
	calls1 := []schemas.ToolCall{{Name: "search", Arguments: map[string]any{"q": "alpha"}}}
	calls2 := []schemas.ToolCall{{Name: "search", Arguments: map[string]any{"q": "beta"}}}
	results := []string{longResult}

	hash1, _ := computeTurnHash(calls1, results)
	hash2, _ := computeTurnHash(calls2, results)
	if hash1 == hash2 {
		t.Errorf("same tool with different args should produce different hashes: hash1=%d hash2=%d", hash1, hash2)
	}
}

func TestConsecutiveRepeatTracking(t *testing.T) {
	// C3 core fix: consecutive-only tracking. Simulate the logic from
	// spawner_dospawn.go to verify it doesn't false-positive on non-consecutive
	// repeats (e.g., polling every 5 turns).
	//
	// Scenario: model polls check_status every 5 turns. Same hash appears on
	// turns 0, 5, 10, 15 — but with different work in between. The old
	// cumulative counter would hit 4 > maxDuplicateResults(3) and trigger.
	// The new consecutive counter never exceeds 1 because the hash changes
	// between polls.
	longResult := strings.Repeat("status: running\n", 10)
	pollCalls := []schemas.ToolCall{{Name: "check_status", Arguments: map[string]any{}}}

	var lastHash uint64
	var consecutive int
	const maxConsecutive = 3

	// Turn 0: poll (hash A)
	h, track := computeTurnHash(pollCalls, []string{longResult})
	if !track {
		t.Fatal("long result should be tracked")
	}
	if h == lastHash {
		consecutive++
	} else {
		lastHash = h
		consecutive = 1
	}
	if consecutive > maxConsecutive {
		t.Errorf("false positive on turn 0: consecutive=%d", consecutive)
	}

	// Turns 1-4: different work each turn (hash B, C, D, E) — resets consecutive
	for i := 1; i <= 4; i++ {
		workCalls := []schemas.ToolCall{{Name: "do_work", Arguments: map[string]any{"step": i}}}
		workResult := strings.Repeat("work done step "+string(rune('0'+i))+"\n", 10)
		h2, _ := computeTurnHash(workCalls, []string{workResult})
		if h2 == lastHash {
			consecutive++
		} else {
			lastHash = h2
			consecutive = 1
		}
		if consecutive > maxConsecutive {
			t.Errorf("false positive on turn %d: consecutive=%d", i, consecutive)
		}
	}

	// Turn 5: poll again (hash A) — consecutive should reset, NOT accumulate
	h, _ = computeTurnHash(pollCalls, []string{longResult})
	if h == lastHash {
		consecutive++
	} else {
		lastHash = h
		consecutive = 1
	}
	if consecutive > maxConsecutive {
		t.Errorf("false positive on turn 5: consecutive=%d", consecutive)
	}
	if consecutive != 1 {
		t.Errorf("non-consecutive repeat should reset to 1, got consecutive=%d", consecutive)
	}
}

func TestConsecutiveRepeatTracking_StuckModelTriggers(t *testing.T) {
	// C3 positive case: model truly stuck making identical calls every turn.
	// After maxConsecutiveDuplicates+1 identical turns, should trigger.
	longResult := strings.Repeat("same data\n", 10)
	calls := []schemas.ToolCall{{Name: "fetch", Arguments: map[string]any{"id": "x"}}}

	var lastHash uint64
	var consecutive int
	const maxConsecutive = 3
	triggered := false

	for turn := 0; turn < 10; turn++ {
		h, track := computeTurnHash(calls, []string{longResult})
		if !track {
			t.Fatal("long result should be tracked")
		}
		if h == lastHash {
			consecutive++
		} else {
			lastHash = h
			consecutive = 1
		}
		if consecutive > maxConsecutive {
			triggered = true
			break
		}
	}

	if !triggered {
		t.Errorf("stuck model (10 identical turns) should trigger after %d consecutive repeats", maxConsecutive+1)
	}
}

func TestPartialStagnationOutput_PreservesWork(t *testing.T) {
	// C3 fix: stagnation failure should return StatusPartial with accumulated
	// trace info, not StatusFailed with all work discarded.
	spawner := &Spawner{}
	trace := []store.ToolInteraction{
		{ToolName: "search", Arguments: map[string]any{"q": "test"}},
		{ToolName: "read_file", Arguments: map[string]any{"path": "/tmp/x"}},
		{ToolName: "fetch_data", Arguments: map[string]any{"id": "abc"}},
	}

	result := spawner.partialStagnationOutput("research", "output_stagnation: test", trace)

	if result.Output.Status != schemas.StatusPartial {
		t.Errorf("status should be StatusPartial, got %s", result.Output.Status)
	}
	if result.Output.Confidence == 0 {
		t.Errorf("confidence should be non-zero (some work was done)")
	}
	toolsUsed, ok := result.Output.Result["tools_used"].([]string)
	if !ok {
		t.Fatalf("tools_used should be []string, got %T", result.Output.Result["tools_used"])
	}
	if len(toolsUsed) != 3 {
		t.Errorf("should preserve 3 tool names, got %d", len(toolsUsed))
	}
	callCount, ok := result.Output.Result["tool_call_count"].(int)
	if !ok {
		t.Fatalf("tool_call_count should be int, got %T", result.Output.Result["tool_call_count"])
	}
	if callCount != 3 {
		t.Errorf("tool_call_count should be 3, got %d", callCount)
	}
}

func TestPartialStagnationOutput_EmptyTrace(t *testing.T) {
	// Edge case: no trace at all.
	spawner := &Spawner{}
	result := spawner.partialStagnationOutput("research", "stagnation", nil)

	if result.Output.Status != schemas.StatusPartial {
		t.Errorf("status should be StatusPartial, got %s", result.Output.Status)
	}
	toolsUsed, _ := result.Output.Result["tools_used"].([]string)
	if len(toolsUsed) != 0 {
		t.Errorf("empty trace should produce empty tools_used, got %d", len(toolsUsed))
	}
}
