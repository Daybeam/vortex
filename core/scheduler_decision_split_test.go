package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_split_test.go — Unit tests for functions moved out of
// scheduler_decision.go into preflight/gate/audit companion files.
// These tests verify the moved functions behave identically to their
// pre-split behavior, ensuring the refactoring introduced no regressions.

// ─── detectUpstreamInsufficient (scheduler_decision_preflight.go) ──────────

func TestDetectUpstreamInsufficient_FailedUpstreamReturnsInfo(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID: "task_upstream_fail",
		Steps:  map[string]*schemas.Step{},
	}
	failedStep := &schemas.Step{ID: "upstream", Status: schemas.StepFailed}
	pendingStep := &schemas.Step{ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}}
	graph.Steps["upstream"] = failedStep
	graph.Steps["downstream"] = pendingStep

	info := engine.detectUpstreamInsufficient(graph)
	if info == nil {
		t.Fatal("expected non-nil info when upstream failed and downstream is pending")
	}
	if info.Step.ID != "downstream" {
		t.Fatalf("expected step ID 'downstream', got %q", info.Step.ID)
	}
	if len(info.UpstreamIDs) != 1 || info.UpstreamIDs[0] != "upstream" {
		t.Fatalf("expected upstream IDs ['upstream'], got %v", info.UpstreamIDs)
	}
	if info.UpstreamStatuses["upstream"] != schemas.StepFailed {
		t.Fatalf("expected upstream status StepFailed, got %v", info.UpstreamStatuses["upstream"])
	}
}

func TestDetectUpstreamInsufficient_AllUpstreamOKReturnsNil(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID: "task_upstream_ok",
		Steps:  map[string]*schemas.Step{},
	}
	okStep := &schemas.Step{ID: "upstream", Status: schemas.StepOK}
	pendingStep := &schemas.Step{ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}}
	graph.Steps["upstream"] = okStep
	graph.Steps["downstream"] = pendingStep

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Fatalf("expected nil when all upstream succeeded, got %+v", info)
	}
}

func TestDetectUpstreamInsufficient_StillRunningReturnsNil(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID: "task_upstream_running",
		Steps:  map[string]*schemas.Step{},
	}
	runningStep := &schemas.Step{ID: "upstream", Status: schemas.StepRunning}
	pendingStep := &schemas.Step{ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}}
	graph.Steps["upstream"] = runningStep
	graph.Steps["downstream"] = pendingStep

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Fatalf("expected nil when upstream still running, got %+v", info)
	}
}

func TestDetectUpstreamInsufficient_NoPendingStepsReturnsNil(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID: "task_no_pending",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
			"s2": {ID: "s2", Status: schemas.StepFailed},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Fatalf("expected nil when no pending steps, got %+v", info)
	}
}

// ─── diagnoseFault (scheduler_decision_preflight.go) ────────────────────────

func TestDiagnoseFault_NilOutputReturnsTransient(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	step := &schemas.Step{ID: "s1"}

	rootCause, suggestion, details := engine.diagnoseFault(nil, step)
	if rootCause != FailureClassTransient {
		t.Fatalf("expected FailureClassTransient, got %v", rootCause)
	}
	if suggestion == "" {
		t.Fatal("expected non-empty suggestion")
	}
	if details != nil {
		t.Fatalf("expected nil details, got %v", details)
	}
}

func TestDiagnoseFault_MissingContextReturnsContextDeficit(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	output := &schemas.SubagentOutput{
		MissingContext: []string{"missing_var"},
	}
	step := &schemas.Step{ID: "s1"}

	rootCause, _, details := engine.diagnoseFault(output, step)
	if rootCause != FailureClassContextDeficit {
		t.Fatalf("expected FailureClassContextDeficit, got %v", rootCause)
	}
	if len(details) != 1 || details[0] != "missing_var" {
		t.Fatalf("expected details ['missing_var'], got %v", details)
	}
}

func TestDiagnoseFault_CapabilityRequiredReturnsCapabilityRequired(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	output := &schemas.SubagentOutput{
		Status:               schemas.StatusCapabilityRequired,
		RequiredCapabilities: []string{"tool_x"},
	}
	step := &schemas.Step{ID: "s1"}

	rootCause, _, details := engine.diagnoseFault(output, step)
	if rootCause != FailureClassCapabilityRequired {
		t.Fatalf("expected FailureClassCapabilityRequired, got %v", rootCause)
	}
	if len(details) != 1 || details[0] != "tool_x" {
		t.Fatalf("expected details ['tool_x'], got %v", details)
	}
}

func TestDiagnoseFault_LowConfidenceReturnsGenerativeUncertainty(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	output := &schemas.SubagentOutput{
		Confidence: 0.3,
	}
	step := &schemas.Step{ID: "s1"}

	rootCause, _, _ := engine.diagnoseFault(output, step)
	if rootCause != FailureClassGenerativeUncertainty {
		t.Fatalf("expected FailureClassGenerativeUncertainty, got %v", rootCause)
	}
}

func TestDiagnoseFault_HighConfidenceReturnsExecutionFailed(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	output := &schemas.SubagentOutput{
		Confidence: 0.8,
	}
	step := &schemas.Step{ID: "s1"}

	rootCause, _, _ := engine.diagnoseFault(output, step)
	if rootCause != "execution_failed" {
		t.Fatalf("expected 'execution_failed', got %v", rootCause)
	}
}

// ─── applyStepFailurePolicy (scheduler_decision_preflight.go) ───────────────

func TestApplyStepFailurePolicy_NilPolicyReturnsFalse(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	graph := &schemas.TaskGraph{TaskID: "t1"}
	step := &schemas.Step{ID: "s1"}

	applied := engine.applyStepFailurePolicy(graph, step, "t1", FailureClassTransient)
	if applied {
		t.Fatal("expected false when FailurePolicy is nil")
	}
}

func TestApplyStepFailurePolicy_SkipSetsStepSkipped(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	graph := &schemas.TaskGraph{TaskID: "t1"}
	step := &schemas.Step{
		ID:            "s1",
		FailurePolicy: &schemas.FailurePolicy{OnFailure: "skip"},
	}

	applied := engine.applyStepFailurePolicy(graph, step, "t1", FailureClassTransient)
	if !applied {
		t.Fatal("expected true when FailurePolicy is skip")
	}
	if step.Status != schemas.StepSkipped {
		t.Fatalf("expected StepSkipped, got %v", step.Status)
	}
}

func TestApplyStepFailurePolicy_AbortSetsGraphFailed(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	graph := &schemas.TaskGraph{TaskID: "t1"}
	step := &schemas.Step{
		ID:            "s1",
		FailurePolicy: &schemas.FailurePolicy{OnFailure: "abort"},
	}

	applied := engine.applyStepFailurePolicy(graph, step, "t1", FailureClassTransient)
	if !applied {
		t.Fatal("expected true when FailurePolicy is abort")
	}
	if graph.Status != schemas.GraphFailed {
		t.Fatalf("expected GraphFailed, got %v", graph.Status)
	}
}

func TestApplyStepFailurePolicy_UnknownPolicyReturnsFalse(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	graph := &schemas.TaskGraph{TaskID: "t1"}
	step := &schemas.Step{
		ID:            "s1",
		FailurePolicy: &schemas.FailurePolicy{OnFailure: "unknown_action"},
	}

	applied := engine.applyStepFailurePolicy(graph, step, "t1", FailureClassTransient)
	if applied {
		t.Fatal("expected false for unknown policy value")
	}
}

// ─── maybeBlock (scheduler_decision_gate.go) ────────────────────────────────

func TestMaybeBlock_WithFallbackActivatesFallback(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_maybe_block_fb",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	failedStep := &schemas.Step{ID: "s1", Status: schemas.StepFailed}
	fallbackStep := &schemas.Step{ID: "s1_fb", Status: schemas.StepSkipped, FallbackFor: "s1"}
	graph.Steps["s1"] = failedStep
	graph.Steps["s1_fb"] = fallbackStep

	engine.maybeBlock(graph, failedStep)

	if fallbackStep.Status != schemas.StepPending {
		t.Fatalf("expected fallback step to be activated (StepPending), got %v", fallbackStep.Status)
	}
	if len(graph.PendingDecisions) != 0 {
		t.Fatalf("expected no decisions when fallback exists, got %d", len(graph.PendingDecisions))
	}
}

func TestMaybeBlock_NoFallbackBlocksDependentsAndCreatesDecision(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_maybe_block_nofb",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	failedStep := &schemas.Step{ID: "s1", Status: schemas.StepFailed}
	dependentStep := &schemas.Step{ID: "s2", Status: schemas.StepPending, DependsOn: []string{"s1"}}
	graph.Steps["s1"] = failedStep
	graph.Steps["s2"] = dependentStep

	engine.maybeBlock(graph, failedStep)

	if dependentStep.Status != schemas.StepBlocked {
		t.Fatalf("expected dependent step to be blocked, got %v", dependentStep.Status)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}
}

// ─── addDecision (scheduler_decision_gate.go) ───────────────────────────────

func TestAddDecision_CreatesDecisionWithCorrectType(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_add_decision",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepFailed}
	graph.Steps["s1"] = step

	engine.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"root_cause": "execution_failed",
	}, []string{"skip", "abort"})

	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	dec := graph.PendingDecisions[0]
	if dec.Type != schemas.DecisionStepFailed {
		t.Fatalf("expected DecisionStepFailed, got %v", dec.Type)
	}
	if dec.StepID != "s1" {
		t.Fatalf("expected StepID 's1', got %q", dec.StepID)
	}
	if graph.Status != schemas.GraphBlocked {
		t.Fatalf("expected GraphBlocked, got %v", graph.Status)
	}
}

func TestAddDecision_UpstreamInsufficientInjectsRewriteDAGOption(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_add_decision_upstream",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepBlocked}
	graph.Steps["s1"] = step

	engine.addDecision(graph, step, schemas.DecisionUpstreamInsufficient, nil, []string{"skip"})

	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	dec := graph.PendingDecisions[0]
	foundRewriteDAG := false
	for _, opt := range dec.Options {
		if opt == "rewrite_dag" {
			foundRewriteDAG = true
			break
		}
	}
	if !foundRewriteDAG {
		t.Fatalf("expected 'rewrite_dag' option for upstream_insufficient, got options: %v", dec.Options)
	}
}

// ─── RequestAutonomousAbort (scheduler_decision_gate.go) ────────────────────

func TestRequestAutonomousAbort_CreatesAbortDecision(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_abort_test",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	engine.graphs = map[string]*schemas.TaskGraph{
		"task_abort_test": graph,
	}

	engine.RequestAutonomousAbort("task_abort_test", "s1", FailureClassCostOverrun, "budget exceeded")

	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	dec := graph.PendingDecisions[0]
	if dec.Type != schemas.DecisionAutonomousAbortRequested {
		t.Fatalf("expected DecisionAutonomousAbortRequested, got %v", dec.Type)
	}
	if dec.StepID != "s1" {
		t.Fatalf("expected StepID 's1', got %q", dec.StepID)
	}
}

func TestRequestAutonomousAbort_NilGraphDoesNothing(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)
	engine.graphs = map[string]*schemas.TaskGraph{}

	engine.RequestAutonomousAbort("nonexistent_task", "s1", FailureClassCostOverrun, "test")

	// Should not panic and should not create any decisions
}

func TestRequestAutonomousAbort_NilStepDoesNothing(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_abort_nil_step",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	engine.graphs = map[string]*schemas.TaskGraph{
		"task_abort_nil_step": graph,
	}

	engine.RequestAutonomousAbort("task_abort_nil_step", "nonexistent_step", FailureClassCostOverrun, "test")

	if len(graph.PendingDecisions) != 0 {
		t.Fatalf("expected 0 decisions for nonexistent step, got %d", len(graph.PendingDecisions))
	}
}
