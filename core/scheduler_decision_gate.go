package core

import (
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_gate.go — Phase 4: Decision gate and autonomous abort.
// Extracted from scheduler_decision.go per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md.
// Contains: addDecision, RequestAutonomousAbort, maybeBlock.

func (s *DirectedEngine) maybeBlock(graph *schemas.TaskGraph, step *schemas.Step) {
	fallback := graph.FallbackFor(step.ID)
	if fallback != nil {
		fallback.Status = schemas.StepPending
		return
	}
	// Block any dependents
	for _, other := range graph.Steps {
		for _, dep := range other.DependsOn {
			if dep == step.ID && other.Status == schemas.StepPending {
				other.Status = schemas.StepBlocked
			}
		}
	}
	s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"last_error":  step.LastError,
		"retry_count": step.RetryCount,
	}, []string{"skip", "abort"})
}

func (s *DirectedEngine) addDecision(
	graph *schemas.TaskGraph,
	step *schemas.Step,
	dtype schemas.DecisionType,
	ctx map[string]any,
	options []string,
) {
	// Inject available models for dynamic escalation (MADMDE)
	if ctx == nil {
		ctx = make(map[string]any)
	}
	if s.registry != nil {
		ctx["available_models"] = s.registry.GetHealthyModelsSummary()
	}

	// [CORE: DAG Surgery Option Injection] (2026-09-07)
	// When the decision type is upstream_insufficient or step_failed with
	// context_deficit, add "rewrite_dag" as a valid option. This allows
	// the Autonomous Decision Router to trigger DAG Surgery automatically.
	if dtype == schemas.DecisionUpstreamInsufficient {
		options = append(options, "rewrite_dag")
	} else if dtype == schemas.DecisionStepFailed {
		rootCause, _ := ctx["root_cause"].(string)
		if rootCause == "context_deficit" || rootCause == "capability_required" {
			options = append(options, "rewrite_dag")
		}
	}

	dec := &schemas.Decision{
		ID:      schemas.NewDecisionID(),
		StepID:  step.ID,
		Type:    dtype,
		Context: ctx,
		Options: options,
		Created: time.Now(),
	}
	// [CORE: Autonomous Decision Router]
	// If the decision context identifies a known retriable failure,
	// automatically route the decision choice to optimize flow,
	// rather than blocking for manual user input.
	if dtype == schemas.DecisionStepFailed {
		if rootCause, ok := ctx["root_cause"].(string); ok && rootCause == "generative_uncertainty" {
			// Automatically route 'refine_and_retry' to bypass human block
			s.SubmitDecision(graph.TaskID, dec.ID, "refine_and_retry")
			return
		}
	}
	// Race fix: protect graph.Status and graph.PendingDecisions with Mu.
	// Must unlock before broadcastDone (line 87) which takes Mu.Lock —
	// sync.RWMutex is not reentrant.
	s.Mu.Lock()
	graph.PendingDecisions = append(graph.PendingDecisions, dec)
	graph.Status = schemas.GraphBlocked
	s.Mu.Unlock()

	// Persist the blocked state immediately to prevent restart loops
	s.persistGraph(graph)

	// Notify waiters that task is blocked (terminal for waiting purposes)
	s.broadcastDone(graph.TaskID)

	s.logger.Log(EventDecisionRequired, graph.TaskID, step.ID, map[string]any{
		"decision_id": dec.ID,
		"type":        dtype,
		"options":     options,
	})
	s.publishEvent(graph.TaskID, step.ID, EventDecisionRequired, map[string]any{
		"decision_id": dec.ID,
		"type":        string(dtype),
	})
}

// RequestAutonomousAbort lets the Main Agent request a structured task abort
// (cost governance, dead-loop detection, etc.). It does NOT cancel the task —
// it creates a human-in-the-loop decision. The user confirms or rejects.
//
// Design ref: docs/architecture/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §3.3-3.4.
// Main Agent has recommendation power; user has approval power.
func (s *DirectedEngine) RequestAutonomousAbort(taskID, stepID string, failureClass FailureClass, reason string) {
	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()
	if graph == nil {
		return
	}
	step := graph.Steps[stepID]
	if step == nil {
		return
	}
	s.addDecision(graph, step, schemas.DecisionAutonomousAbortRequested, map[string]any{
		"failure_class": string(failureClass),
		"reason":        reason,
		"requested_by":  "main_agent",
	}, []string{"confirm_abort", "continue"})
}
