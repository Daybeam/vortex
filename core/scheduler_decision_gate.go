package core

import (
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_gate.go — Phase 4: Decision gate and autonomous abort.
// Extracted from scheduler_decision.go per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md.
// Contains: addDecision, RequestAutonomousAbort, maybeBlock,
// handleDelegationRequired, handleDefaultFailure.

func (s *DirectedEngine) handleDelegationRequired(graph *schemas.TaskGraph, step *schemas.Step, output schemas.SubagentOutput) {
	s.Mu.Lock()
	step.Status = schemas.StepBlocked
	s.Mu.Unlock()
	s.addDecision(graph, step, schemas.DecisionDelegationRequired, map[string]any{
		"prompt":  output.Result,
		"role_id": step.RoleID,
	}, []string{"fulfill", "skip", "abort"})
}

func (s *DirectedEngine) handleDefaultFailure(graph *schemas.TaskGraph, step *schemas.Step) {
	s.Mu.Lock()
	step.Status = schemas.StepFailed // audit C-9: protect 2-word string write
	s.Mu.Unlock()
	s.maybeBlock(graph, step)
}

func (s *DirectedEngine) maybeBlock(graph *schemas.TaskGraph, step *schemas.Step) {
	s.Mu.Lock()
	fallback := graph.FallbackFor(step.ID)
	if fallback != nil {
		fallback.Status = schemas.StepPending // audit C-9
		s.Mu.Unlock()
		return
	}
	// Block any dependents — iterate graph.Steps under Lock (MutateGraphTopology can add entries)
	for _, other := range graph.Steps {
		for _, dep := range other.DependsOn {
			if dep == step.ID && other.Status == schemas.StepPending {
				other.Status = schemas.StepBlocked // audit C-9
			}
		}
	}
	s.Mu.Unlock() // release before addDecision which takes its own Lock
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
	// context_deficit, add "rewrite_dag" as a valid option for the external
	// decider (or E5 NonInteractive auto-resolver) to choose.
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
	// NOTE: The former "Autonomous Decision Router" that auto-routed
	// generative_uncertainty → refine_and_retry was removed (2026-10-02).
	// It called SubmitDecision BEFORE the decision was enqueued, so the
	// call always failed silently, and the early return prevented the
	// decision from being added to PendingDecisions — causing silent
	// deadlocks. The E5 NonInteractive auto-resolution below replaces it.
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

	// E5 fix (2026-10-02): NonInteractive auto-resolution.
	// In unattended mode (CI / batch eval), a decision that would block forever
	// is auto-resolved, using the SAME SubmitDecision path an external caller
	// uses (reuses pending-removal + run() restart). Runs in a goroutine so the
	// current step goroutine can unwind first — SubmitDecision cancels the run
	// ctx, which would otherwise abort this step mid-flight. Config-gated; default off.
	//
	// Two strategies:
	//   delegate    — DecisionDeciderRole is set: spawn that role to evaluate
	//                 the decision context and choose. Falls back to conservative
	//                 if the decider errors or returns an invalid choice.
	//   conservative — no decider role: use defaultNonInteractiveChoice (hardcoded
	//                  safe defaults). Guaranteed to terminate.
	if s.registry != nil && s.registry.System.NonInteractive {
		taskID, decID := graph.TaskID, dec.ID
		deciderRole := s.registry.System.DecisionDeciderRole
		s.logger.Log(EventDecisionAutoResolved, taskID, step.ID, map[string]any{
			"decision_id":  decID,
			"type":         string(dtype),
			"options":      options,
			"decider_role": deciderRole,
		})
		s.goBackground(func() {
			choice := s.resolveNonInteractiveDecision(deciderRole, taskID, decID, step.ID, dtype, ctx, options)
			if choice == "" {
				return // must not auto-resolve (e.g. human_approval_required)
			}
			if err := s.SubmitDecision(taskID, decID, choice); err != nil {
				s.logger.Log(EventDecisionAutoResolved, taskID, step.ID, map[string]any{
					"decision_id":        decID,
					"auto_resolve_error": err.Error(),
				})
			}
		})
	}
}

// resolveNonInteractiveDecision picks a choice for a blocked decision in
// unattended mode. If deciderRole is non-empty, it spawns that role to
// evaluate the decision and choose; on any failure it falls back to the
// conservative default. Returns "" if the decision must not be auto-resolved.
func (s *DirectedEngine) resolveNonInteractiveDecision(
	deciderRole, taskID, decID, stepID string,
	dtype schemas.DecisionType,
	ctx map[string]any,
	options []string,
) string {
	// Delegate strategy: spawn decider role to choose.
	if deciderRole != "" && s.spawner != nil {
		if choice, ok := s.spawnDecider(deciderRole, taskID, stepID, dtype, ctx, options); ok {
			return choice
		}
		// Decider failed or returned invalid choice — fall through to conservative.
		s.logger.Log(EventDecisionAutoResolved, taskID, stepID, map[string]any{
			"decision_id": decID,
			"fallback":    "decider_failed_or_invalid",
		})
	}
	// Conservative strategy: hardcoded safe defaults.
	choice, ok := defaultNonInteractiveChoice(dtype, options)
	if !ok {
		return ""
	}
	return choice
}

// spawnDecider asks the configured decider role to choose an option for a
// blocked decision. Returns (choice, true) if the decider picked a valid
// option, or ("", false) on any error or invalid response.
func (s *DirectedEngine) spawnDecider(
	roleID, taskID, stepID string,
	dtype schemas.DecisionType,
	ctx map[string]any,
	options []string,
) (string, bool) {
	prompt := fmt.Sprintf(`You are a decision arbiter. A workflow step has blocked and requires a decision.

Decision type: %s
Decision context: %v
Available options: %s

Choose exactly ONE option from the list above. Respond with ONLY the option name, nothing else.`,
		dtype, ctx, strings.Join(options, ", "))

	res, err := s.spawner.Spawn(s.lifecycleCtx, &SpawnRequest{
		TaskID:      taskID,
		StepID:      stepID + "_decider",
		RoleID:      roleID,
		Task:        prompt,
		RoutingMode: schemas.RoutingModeLegacy,
	})
	if err != nil || res == nil {
		// audit L-N10: Spawn can return (nil, nil) e.g. when an external
		// interceptor short-circuits. Without this guard, res.Output.Result
		// panics with nil deref — the goroutine exits silently via
		// goBackground's recover, and the decision deadlocks forever in
		// NonInteractive mode (no human to resolve it).
		return "", false
	}
	text := strings.TrimSpace(strings.ToLower(fmt.Sprintf("%v", res.Output.Result)))
	for _, opt := range options {
		if strings.ToLower(opt) == text {
			return opt, true
		}
	}
	// Fuzzy: response contains the option as a substring.
	for _, opt := range options {
		if strings.Contains(text, strings.ToLower(opt)) {
			return opt, true
		}
	}
	return "", false
}

// askSystemOneForDecision uses the System One decision model to pick an option.
// System One is not available in the open core; this stub always returns false.
func (s *DirectedEngine) askSystemOneForDecision(
	taskID, stepID string,
	dtype schemas.DecisionType,
	decisionCtx map[string]any,
	options []string,
) (string, bool) {
	return "", false
}

// defaultNonInteractiveChoice returns the conservative default choice to use
// when NonInteractive mode must resolve a decision without a human, or
// ("", false) if the decision type must NOT be auto-resolved.
//
// Conservative bias: prefer "skip" (don't kill the whole task, don't loop).
// human_approval_required is deliberately excluded — it is an explicit safety
// gate and must always wait for a human, even unattended.
func defaultNonInteractiveChoice(dtype schemas.DecisionType, options []string) (string, bool) {
	var want string
	switch dtype {
	case schemas.DecisionStepFailed, schemas.DecisionCapabilityRequired,
		schemas.DecisionLowConfidence, schemas.DecisionEnvironmentMissing,
		schemas.DecisionDelegationRequired, schemas.DecisionUpstreamInsufficient,
		schemas.DecisionBudgetExhausted:
		want = "skip"
	case schemas.DecisionMaxTurnsExhausted:
		// Bounded by the existing TurnsBudgetBonus cap; grants the stuck step
		// one more chunk of turns rather than silently skipping it.
		want = "resume_more_turns"
	case schemas.DecisionAutonomousAbortRequested:
		// Cost-governance abort request: in unattended mode, "continue" keeps
		// running (the budget sentinel fired once per tier, so this is bounded).
		want = "continue"
	default:
		// human_approval_required, rewrite_dag (needs a surgery payload), etc.
		return "", false
	}
	// The chosen default must actually be an offered option.
	for _, o := range options {
		if o == want {
			return want, true
		}
	}
	return "", false
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
	if graph == nil {
		s.Mu.RUnlock()
		return
	}
	step := graph.Steps[stepID] // audit C-4: map read must be under RLock (MutateGraphTopology can add steps)
	s.Mu.RUnlock()
	if step == nil {
		return
	}
	s.addDecision(graph, step, schemas.DecisionAutonomousAbortRequested, map[string]any{
		"failure_class": string(failureClass),
		"reason":        reason,
		"requested_by":  "main_agent",
	}, []string{"confirm_abort", "continue"})
}
