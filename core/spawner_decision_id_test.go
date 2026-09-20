package core

import (
	"testing"
)

// Regression tests for M2: _decision_id not leaked into MCP tool arguments.
//
// Before the fix, interaction.Arguments shared the same map reference as
// call.Arguments. Adding _decision_id to the interaction trace also added it
// to call.Arguments, which was then sent to the MCP server as part of the
// tool call payload — leaking internal orchestration metadata into external
// tool calls. The fix copies call.Arguments into a new map for the
// interaction trace, so _decision_id stays in the trace only.
//
// The copying logic was extracted into copyToolCallArguments (spawner_utils.go)
// to make it unit-testable without needing a full Spawner + provider setup.

// TestCopyToolCallArguments_DecisionIDNotLeakedIntoSource verifies the core M2
// invariant: after copying and stamping _decision_id, the original source map
// must NOT contain _decision_id.
func TestCopyToolCallArguments_DecisionIDNotLeakedIntoSource(t *testing.T) {
	src := map[string]any{
		"tool_arg": "value",
		"count":    42,
	}
	decisionID := "decision-abc-123"

	dst := copyToolCallArguments(src, decisionID)

	// The copy must have _decision_id.
	if got := dst["_decision_id"]; got != decisionID {
		t.Fatalf("expected dst[_decision_id]=%q, got %v", decisionID, got)
	}

	// The original source must NOT have _decision_id — this is the bug the
	// fix prevents. If this assertion fails, _decision_id is leaking into
	// the MCP tool call payload.
	if _, leaked := src["_decision_id"]; leaked {
		t.Fatal("_decision_id leaked into the source map — the MCP server would receive internal orchestration metadata in the tool call payload")
	}
}

// TestCopyToolCallArguments_PreservesOriginalArgs verifies that all original
// arguments from the source map are present in the copy.
func TestCopyToolCallArguments_PreservesOriginalArgs(t *testing.T) {
	src := map[string]any{
		"path":    "/tmp/test.txt",
		"content": "hello world",
		"mode":    0644,
	}

	dst := copyToolCallArguments(src, "dec-1")

	for k, expected := range src {
		if got := dst[k]; got != expected {
			t.Errorf("dst[%q] = %v, want %v", k, got, expected)
		}
	}
}

// TestCopyToolCallArguments_EmptyDecisionID_NoStamp verifies that when
// decisionID is empty, _decision_id is not added to the copy at all.
func TestCopyToolCallArguments_EmptyDecisionID_NoStamp(t *testing.T) {
	src := map[string]any{"arg": "val"}

	dst := copyToolCallArguments(src, "")

	if _, present := dst["_decision_id"]; present {
		t.Fatal("_decision_id should not be present when decisionID is empty")
	}
}

// TestCopyToolCallArguments_NilSource_NoPanic verifies that a nil source map
// is handled gracefully (produces a valid, non-nil destination with at most
// the _decision_id key).
func TestCopyToolCallArguments_NilSource_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("copyToolCallArguments panicked on nil source: %v", r)
		}
	}()

	dst := copyToolCallArguments(nil, "dec-1")
	if dst == nil {
		t.Fatal("expected non-nil destination even for nil source")
	}
	if got := dst["_decision_id"]; got != "dec-1" {
		t.Fatalf("expected dst[_decision_id]=%q, got %v", "dec-1", got)
	}
}

// TestCopyToolCallArguments_IndependentMaps verifies that modifying the
// destination map does not affect the source map (true copy, not a reference).
func TestCopyToolCallArguments_IndependentMaps(t *testing.T) {
	src := map[string]any{"key": "original"}

	dst := copyToolCallArguments(src, "dec-1")
	dst["key"] = "modified"
	dst["new_key"] = "new"

	if src["key"] != "original" {
		t.Fatalf("modifying dst changed src[key] to %v — maps are not independent", src["key"])
	}
	if _, present := src["new_key"]; present {
		t.Fatal("adding a key to dst leaked into src — maps are not independent")
	}
}
