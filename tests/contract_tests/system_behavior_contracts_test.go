package contract_tests

import (
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/store"
)

// TestSystemContract_LoopDetection verifies the project's Sieve
// (core/sieve.go) detects infinite tool-call loops.
//
// This replaces the deleted behavior_contracts_test.go which tested
// a toy SieveGuardian mock instead of the project's real Sieve.
func TestSystemContract_LoopDetection(t *testing.T) {
	sieve := core.NewSieve(10) // maxKeep=10, default ToolRepetitionThreshold=3

	taskID := "task-loop-test"
	toolCall := store.ToolInteraction{
		ToolName:  "run_command",
		Arguments: map[string]any{"args": []string{"ls", "-R"}},
	}

	// First 3 calls: should pass (history grows to 3, threshold=3,
	// check counts consecutive entries BEFORE appending current call)
	for i := 1; i <= 3; i++ {
		ok, reason := sieve.InspectToolCalls(taskID, []store.ToolInteraction{toolCall})
		if !ok {
			t.Fatalf("call %d: expected pass, got rejection: %s", i, reason)
		}
		if reason != "" {
			t.Errorf("call %d: expected empty reason, got %s", i, reason)
		}
	}

	// 4th call: consecutiveCount=3 >= threshold=3 → intercepted
	ok, reason := sieve.InspectToolCalls(taskID, []store.ToolInteraction{toolCall})
	if ok {
		t.Fatal("expected 3rd identical call to be intercepted, but it passed")
	}
	if reason != "InfiniteLoopDetected" {
		t.Errorf("expected reason 'InfiniteLoopDetected', got %q", reason)
	}
}

// TestSystemContract_LoopDetection_DifferentTools verifies the Sieve
// does NOT flag distinct tool calls as a loop.
func TestSystemContract_LoopDetection_DifferentTools(t *testing.T) {
	sieve := core.NewSieve(10)

	taskID := "task-diff-tools"
	calls := []store.ToolInteraction{
		{ToolName: "read_file", Arguments: map[string]any{"path": "a.go"}},
		{ToolName: "read_file", Arguments: map[string]any{"path": "b.go"}},
		{ToolName: "read_file", Arguments: map[string]any{"path": "c.go"}},
	}

	// All 3 calls have different arguments → not a loop
	ok, reason := sieve.InspectToolCalls(taskID, calls)
	if !ok {
		t.Errorf("expected distinct calls to pass, got rejection: %s", reason)
	}
}

// TestSystemContract_LoopDetection_EmptyCalls verifies the Sieve
// handles empty input gracefully.
func TestSystemContract_LoopDetection_EmptyCalls(t *testing.T) {
	sieve := core.NewSieve(10)

	ok, reason := sieve.InspectToolCalls("task-empty", nil)
	if !ok {
		t.Errorf("expected empty calls to pass, got rejection: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason for empty calls, got %s", reason)
	}
}

// TestSystemContract_SieveContextCompression verifies the project's Sieve
// can compress context data without panicking on nil/empty input.
func TestSystemContract_SieveContextCompression(t *testing.T) {
	sieve := core.NewSieve(10)

	// Empty map should not panic
	result := sieve.CompressContext(map[string]any{})
	if result == nil {
		t.Error("expected non-nil result for empty context compression")
	}

	// Nested data should not panic
	result = sieve.CompressContext(map[string]any{
		"level1": map[string]any{
			"level2": "value",
		},
	})
	if result == nil {
		t.Error("expected non-nil result for nested context compression")
	}
}
