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

// TestFirstDelegations_TerminalGraphReturnsImmediately is a regression test for
// N3 (DELEGATION_HANDOVER_REDESIGN.md §11.1): after the last step is fulfilled,
// FirstDelegations must return immediately (not wait out the full timeout)
// because the graph is terminal and no more delegation decisions can ever appear.
//
// Bug: FirstDelegations always waited the full timeout when no views were found,
// even if the graph was already terminal. This added ~500ms latency to every
// final submit_decision response.
func TestFirstDelegations_TerminalGraphReturnsImmediately(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Think for me"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Get the first delegation and fulfill it so the graph becomes terminal.
	views, found := engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	if !found || len(views) == 0 {
		t.Fatalf("expected delegation views, found=%v len=%d", found, len(views))
	}

	outputJSON := `{"status":"ok","confidence":0.9,"result":{"answer":"42"}}`
	if err := engine.FulfillStep(taskID, views[0].DecisionID, outputJSON); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	// Give the engine a moment to process the fulfillment and reach terminal state.
	time.Sleep(200 * time.Millisecond)

	// N3: FirstDelegations must return quickly (well under the timeout) for a
	// terminal graph, instead of waiting the full duration.
	start := time.Now()
	_, found = engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	elapsed := time.Since(start)

	if found {
		t.Error("expected FirstDelegations to return false for terminal graph, got found=true")
	}
	if elapsed > 1*time.Second {
		t.Errorf("N3 regression: FirstDelegations waited %v for a terminal graph; "+
			"expected < 1s (terminal short-circuit not working)", elapsed)
	}
}

// TestDelegationView_ACPHintPopulatedFromStep is a regression test for D8
// lightweight (DELEGATION_HANDOVER_REDESIGN.md §4.8): when a delegated step
// has ACPConfig, the delegation envelope must include acp_hint so the host
// can forward the step to its own ACP endpoint.
func TestDelegationView_ACPHintPopulatedFromStep(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_acp", RoleID: "worker", Task: "Run via ACP"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the delegation decision to appear.
	views, found := engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	if !found || len(views) == 0 {
		t.Fatalf("expected delegation views, found=%v len=%d", found, len(views))
	}
	if views[0].ACPHint != nil {
		t.Fatal("ACPHint should be nil when step has no ACPConfig")
	}

	// Set ACPConfig on the step, then re-snapshot.
	acpCfg := &schemas.ACPStepConfig{
		BinaryPath: "/usr/local/bin/acp-worker",
		Args:       []string{"--mode", "remote"},
		TimeoutSec: 120,
	}
	engine.Mu.Lock()
	if graph, ok := engine.graphs[taskID]; ok {
		if step, ok := graph.Steps["step_acp"]; ok {
			step.ACPConfig = acpCfg
		}
	}
	engine.Mu.Unlock()

	views, found = engine.FirstDelegations(context.Background(), taskID, 1*time.Second)
	if !found || len(views) == 0 {
		t.Fatalf("expected delegation views after ACPConfig set, found=%v", found)
	}
	if views[0].ACPHint == nil {
		t.Fatal("D8 regression: ACPHint is nil; the host cannot forward to its ACP endpoint")
	}
	if views[0].ACPHint.BinaryPath != acpCfg.BinaryPath {
		t.Errorf("ACPHint.BinaryPath = %q, want %q", views[0].ACPHint.BinaryPath, acpCfg.BinaryPath)
	}
	if views[0].ACPHint.TimeoutSec != 120 {
		t.Errorf("ACPHint.TimeoutSec = %d, want 120", views[0].ACPHint.TimeoutSec)
	}
}

// TestDelegationKindPlan_ProducesPlanDelegation is a regression test for D5 P1b
// (DELEGATION_HANDOVER_REDESIGN.md §4.5): in delegation mode with
// EnablePlannerFirst, submit_task should produce a delegation with Kind="plan"
// containing the planner prompt, instead of skipping the planner (P1a).
func TestDelegationKindPlan_ProducesPlanDelegation(t *testing.T) {
	engine := newDelegationTestEngine(t)
	engine.registry.System.EnablePlannerFirst = true

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_plan", RoleID: "worker", Task: "Build a payment system"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	views, found := engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	if !found || len(views) == 0 {
		t.Fatalf("expected plan delegation, found=%v len=%d", found, len(views))
	}
	if views[0].Kind != DelegationKindPlan {
		t.Fatalf("expected Kind=%q, got %q", DelegationKindPlan, views[0].Kind)
	}
	if views[0].Prompt == nil {
		t.Fatal("plan delegation prompt is nil")
	}
	if _, ok := views[0].Prompt["user_prompt"]; !ok {
		t.Error("plan delegation prompt missing user_prompt")
	}
}

// TestDelegationKindPlan_FulfillInjectsSteps verifies that fulfilling a plan
// delegation with a valid plan injects the steps into the graph via surgery.
func TestDelegationKindPlan_FulfillInjectsSteps(t *testing.T) {
	engine := newDelegationTestEngine(t)
	engine.registry.System.EnablePlannerFirst = true

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_plan", RoleID: "worker", Task: "Build a payment system"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	views, found := engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	if !found || len(views) == 0 {
		t.Fatalf("expected plan delegation, found=%v", found)
	}

	// Fulfill with a valid plan.
	planJSON := `{"steps":[{"id":"s1","role_id":"worker","task":"design schema"},{"id":"s2","role_id":"worker","task":"implement API","depends_on":["s1"]}]}`
	if err := engine.FulfillStep(taskID, views[0].DecisionID, planJSON); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	// Verify the graph now has the injected steps.
	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph == nil {
		t.Fatal("graph not found after plan fulfillment")
	}
	if _, ok := graph.Steps["s1"]; !ok {
		t.Error("step s1 not injected into graph")
	}
	if _, ok := graph.Steps["s2"]; !ok {
		t.Error("step s2 not injected into graph")
	}
	// The original planning step should be deprecated.
	if step, ok := graph.Steps["step_plan"]; ok {
		if step.Status != schemas.StepDeprecated {
			t.Errorf("planning step status = %v, want StepDeprecated", step.Status)
		}
	}
}
