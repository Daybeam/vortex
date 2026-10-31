package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_wait_view_test.go — D2 regression tests
// (DELEGATION_HANDOVER_REDESIGN.md §4.1, "顺带" wait_task auto-switch).
//
// Bug: wait_task defaults to view="summary", which only includes
// pending_decision_count (an integer) — not the decision envelopes
// (prompts, options) the host needs to execute the delegated step.
// The fix: the wait_task handler auto-switches to view="full" when
// pending_decision_count > 0. These tests verify the engine behavior
// that enables that switch: summary reports the count, full includes
// the envelopes.

// TestWaitTask_SummaryReportsPendingCount verifies that WaitTask with
// default (summary) view reports pending_decision_count > 0 when
// delegation decisions exist, and does NOT include the full
// pending_decisions array (only the count).
func TestWaitTask_SummaryReportsPendingCount(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the delegation decision to appear.
	waitForDelegationDecision(t, engine, taskID)

	// WaitTask with default summary view (no view arg).
	status, ok := engine.WaitTask(context.Background(), taskID, 2*time.Second)
	if !ok {
		t.Fatalf("WaitTask returned false")
	}

	pdc, _ := status["pending_decision_count"].(int)
	if pdc == 0 {
		t.Fatalf("expected pending_decision_count > 0 in summary view, got 0")
	}

	// Summary view must NOT include the full pending_decisions array —
	// only the count. This is what makes the D2 fix necessary: the host
	// can detect "there are decisions" but can't see what they are.
	if _, hasDetails := status["pending_decisions"]; hasDetails {
		t.Fatalf("summary view should not include pending_decisions details")
	}
}

// TestGetStatus_FullViewIncludesDelegationPrompt verifies that GetStatus
// with view="full" includes pending_decisions with the delegation prompt
// envelope (system_prompt, user_prompt) and the decision options.
// This is what the D2 fix's auto-switch to full view delivers to the host.
func TestGetStatus_FullViewIncludesDelegationPrompt(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	// GetStatus with full view.
	status, ok := engine.GetStatus(taskID, "full")
	if !ok {
		t.Fatalf("GetStatus returned false")
	}

	decisions, _ := status["pending_decisions"].([]map[string]any)
	if len(decisions) == 0 {
		t.Fatalf("expected pending_decisions in full view, got empty or wrong type")
	}

	// Verify the decision ID matches the one we waited for.
	if decisions[0]["id"] != dec.ID {
		t.Fatalf("expected decision ID %s, got %v", dec.ID, decisions[0]["id"])
	}

	// Verify the decision contains the prompt envelope.
	ctxMap, _ := decisions[0]["context"].(map[string]any)
	if ctxMap == nil {
		t.Fatalf("expected context map in decision, got nil")
	}
	prompt, _ := ctxMap["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatalf("expected prompt envelope in decision context, got nil")
	}
	if _, ok := prompt["system_prompt"]; !ok {
		t.Fatalf("expected system_prompt in prompt envelope, got nil")
	}
	if _, ok := prompt["user_prompt"]; !ok {
		t.Fatalf("expected user_prompt in prompt envelope, got nil")
	}

	// Verify options include "fulfill" (the key delegation action).
	opts, _ := decisions[0]["options"].([]string)
	foundFulfill := false
	for _, o := range opts {
		if o == "fulfill" {
			foundFulfill = true
			break
		}
	}
	if !foundFulfill {
		t.Fatalf("expected 'fulfill' in decision options, got %v", opts)
	}
}
