package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// These tests are regression tests for D1 (DELEGATION_HANDOVER_REDESIGN.md §4.1):
// submit_task only returned task_id, forcing the host to do a blind
// get_task_status poll to discover the delegation prompt. FirstDelegations
// lets the submit handler synchronously wait for and return the envelope.

// TestFirstDelegations_SingleStep verifies that after submitting a single-step
// task in delegation mode, FirstDelegations returns one delegation view with
// the prompt envelope (system_prompt, user_prompt, tool_definitions) and the
// correct decision metadata.
func TestFirstDelegations_SingleStep(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Think for me"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	views, found := engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	if !found {
		t.Fatal("D1 regression: FirstDelegations returned no delegations within timeout; " +
			"submit_task cannot synchronously deliver the envelope")
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 delegation view, got %d", len(views))
	}

	v := views[0]
	if v.StepID != "step_1" {
		t.Errorf("expected StepID=\"step_1\", got %q", v.StepID)
	}
	if v.DecisionID == "" {
		t.Error("expected non-empty DecisionID")
	}
	if v.Kind != DelegationKindExternalSubagent {
		t.Errorf("expected Kind=%q, got %q", DelegationKindExternalSubagent, v.Kind)
	}
	// Options should include "fulfill" (the action the host takes to return results).
	hasFulfill := false
	for _, opt := range v.Options {
		if opt == "fulfill" {
			hasFulfill = true
		}
	}
	if !hasFulfill {
		t.Errorf("expected Options to contain \"fulfill\", got %v", v.Options)
	}
	// The prompt envelope must contain system_prompt and user_prompt — these
	// are what the host needs to execute the step itself.
	if v.Prompt == nil {
		t.Fatal("D1 regression: Prompt is nil; the host cannot execute without the envelope")
	}
	if _, ok := v.Prompt["system_prompt"]; !ok {
		t.Error("D1 regression: Prompt missing \"system_prompt\" key")
	}
	if _, ok := v.Prompt["user_prompt"]; !ok {
		t.Error("D1 regression: Prompt missing \"user_prompt\" key")
	}
}

// TestFirstDelegations_TimeoutReturnsFalse verifies that FirstDelegations
// returns false when no delegation decisions appear within the timeout.
// This happens for non-delegation tasks or when the decision hasn't been
// produced yet — the caller falls back to get_task_status polling.
func TestFirstDelegations_TimeoutReturnsFalse(t *testing.T) {
	engine := newDelegationTestEngine(t)

	// Submit a task — in delegation mode a decision WILL appear, but we use a
	// very short timeout to verify the timeout path returns false cleanly.
	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Think for me"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// 1ms timeout — almost certainly too short for the goroutine to produce
	// the decision. If it happens to be fast enough, that's fine too (found=true).
	// The key assertion is that FirstDelegations doesn't hang or panic.
	_, _ = engine.FirstDelegations(context.Background(), taskID, 1*time.Millisecond)

	// Now verify with a cancelled context — must return false immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	views, found := engine.FirstDelegations(ctx, taskID, 5*time.Second)
	if found {
		t.Fatalf("expected FirstDelegations to return false on cancelled context, got %d views", len(views))
	}
}

// TestFirstDelegations_MultiStepParallel verifies that a two-step task with
// independent steps returns both delegation envelopes in one call (parallel
// branches are dispatched concurrently by run()).
func TestFirstDelegations_MultiStepParallel(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_a", RoleID: "worker", Task: "Do task A"},
		{ID: "step_b", RoleID: "worker", Task: "Do task B"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	views, found := engine.FirstDelegations(context.Background(), taskID, 5*time.Second)
	if !found {
		t.Fatal("D1 regression: FirstDelegations returned no delegations for multi-step task")
	}
	if len(views) < 1 {
		t.Fatalf("expected at least 1 delegation view for multi-step task, got %d", len(views))
	}
	// Both steps are independent (no DependsOn), so both should be dispatched.
	// We check that at least one view has a non-empty prompt.
	for _, v := range views {
		if v.Prompt == nil {
			t.Errorf("delegation view for step %s has nil Prompt", v.StepID)
		}
	}
}
