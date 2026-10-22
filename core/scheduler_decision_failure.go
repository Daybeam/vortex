package core

import (
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_failure.go — Phase 3: Step failure/partial handling.
// Extracted from handleOutput per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md Step 3.
// Returns VerdictReturn when an early-exit path (backtrack, ASAE, fallback)
// handles the situation; VerdictContinue when it falls through to decision
// blocking.

// emitMountApprovalDecision raises a human_approval_required decision for the
// given mount-policy `approval` blocks (G2, 2026-10-22; skills added D3).
// SubmitDecision "approve" records the mounts on the graph and re-queues the
// step. Extracted from handleStepFailure to keep it within the size ratchet.
func (s *DirectedEngine) emitMountApprovalDecision(graph *schemas.TaskGraph, step *schemas.Step, mounts, skills []string) {
	ctx := map[string]any{
		"message":    "MCP mount requires human approval before this step can run.",
		"role_id":    step.RoleID,
		"root_cause": string(FailureClassApprovalRequired),
	}
	if len(mounts) > 0 {
		ctx["approval_required_mounts"] = mounts
	}
	if len(skills) > 0 {
		ctx["approval_required_skills"] = skills
		ctx["message"] = "MCP/skill mount requires human approval before this step can run."
	}
	s.addDecision(graph, step, schemas.DecisionHumanApprovalRequired, ctx, []string{"approve", "reject"})
}

// reclassifyToolRefusal promotes a generative_uncertainty failure to
// deterministic_tool_refusal when a tool has repeatedly failed at the task
// level (E2/E3 fix, 2026-10-02): a same-role follow-up would just repeat the
// failing call (the amplification loop seen as 21/22 generative_uncertainty).
// Only overrides generative_uncertainty — context_deficit carries a genuine
// missing-context signal that must be preserved. Extracted from
// handleStepFailure to keep that function within the size ratchet.
func (s *DirectedEngine) reclassifyToolRefusal(taskID string, cause FailureClass, suggestion string, details []string) (FailureClass, string, []string) {
	if cause != FailureClassGenerativeUncertainty || s.spawner == nil {
		return cause, suggestion, details
	}
	tool, n := s.spawner.RepeatedToolFailure(taskID)
	if n < toolFailCircuitBreakerThreshold {
		return cause, suggestion, details
	}
	return FailureClassDeterministicToolRefusal,
		fmt.Sprintf("Tool %q failed %d times this task; deterministic or environmental refusal. Do not repeat the same call.", tool, n),
		append(details, tool)
}

func (s *DirectedEngine) handleStepFailure(graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult) DecisionVerdict {
	taskID := graph.TaskID
	output := result.Output
	conf := output.Confidence

	s.Mu.Lock()
	AutoDepositResult(graph, step.ID, output.Result)
	s.Mu.Unlock()

	if s.Archive != nil {
		s.archiveStep(graph, step, output)
	}

	cause, suggestion, details := s.diagnoseFault(&output, step)

	cause, suggestion, details = s.reclassifyToolRefusal(taskID, cause, suggestion, details)

	// G2: an `approval` mount blocks the step on a human decision, not a failure.
	if cause == FailureClassApprovalRequired {
		s.emitMountApprovalDecision(graph, step, output.ApprovalRequiredMounts, output.ApprovalRequiredSkills)
		return VerdictContinue
	}

	if s.tryBacktrack(graph, step, &output) {
		return VerdictReturn
	}

	maxSpawn := s.registry.System.MaxSpawnDepth
	if step.MaxSpawnDepth > 0 {
		maxSpawn = step.MaxSpawnDepth
	}

	if step.SpawnDepth < maxSpawn && (cause == FailureClassContextDeficit || cause == FailureClassGenerativeUncertainty) {
		s.Mu.Lock()
		subStepID := fmt.Sprintf("%s_spawn_%s", step.ID, uuid.New().String()[:4])
		refinedTask := fmt.Sprintf("Autonomous Follow-up Task: Address missing context/uncertainty: %v. Original task: %s",
			output.MissingContext, step.Task)

		surgery := SurgeryData{
			DeprecatedStepID: step.ID,
			ReplacementSteps: []schemas.StepInput{
				{
					ID:            subStepID,
					RoleID:        step.RoleID,
					Task:          refinedTask,
					SpawnDepth:    step.SpawnDepth + 1,
					MaxSpawnDepth: maxSpawn,
				},
			},
		}

		if err := s.mutateGraphTopologyLocked(graph, surgery); err == nil {
			s.Mu.Unlock()
			s.logger.Log("EventAutonomousStepSpawned", taskID, subStepID, map[string]any{
				"parent_step":     step.ID,
				"missing_context": output.MissingContext,
				"spawn_depth":     step.SpawnDepth + 1,
			})
			return VerdictReturn
		}
		s.Mu.Unlock()
	}

	// audit L-N7/L-N11: graph.FallbackFor iterates graph.Steps (map) and we
	// read/write fallback.Status — both race with MutateGraphTopology which
	// adds steps under s.Mu.Lock(). Use a single Lock (not RLock+Lock) to
	// eliminate the TOCTOU window where a concurrent MutateGraphTopology could
	// remove or modify the fallback between RUnlock and Lock.
	s.Mu.Lock()
	fallback := graph.FallbackFor(step.ID)
	if fallback != nil && fallback.Status == schemas.StepPending {
		step.Status = schemas.StepFailed
		step.LastError = fmt.Sprintf("%s: confidence %.2f, root cause: %s", output.Status, conf, string(cause))
		fallback.Status = schemas.StepPending
		s.Mu.Unlock()
		s.logger.Log(EventFallbackActivated, taskID, step.ID, map[string]any{
			"fallback_step": fallback.ID,
			"root_cause":    string(cause),
			"confidence":    conf,
		})
		return VerdictReturn
	}
	s.Mu.Unlock()

	// ── A3 Outcome Probe + A2/A4 Compensation (ADDED 2026-10-13) ───────
	// Design ref: docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.3
	//
	// Before blocking the step, probe external state for all effects the
	// step produced and attempt compensation in reverse order. File effects
	// are delegated to StagedWorkspace (existing rollback in scheduler_dag.go).
	// External effects (api/database/network) are probed and compensated here.
	// Effects that cannot be compensated are marked needs_manual_review.
	if s.spawner != nil {
		tracker := s.spawner.GetOrCreateEffectTracker(taskID)
		compResults := tracker.CompensateStep(step.ID)
		if len(compResults) > 0 {
			for _, cr := range compResults {
				s.logger.Log("EventA3Compensation", taskID, step.ID, map[string]any{
					"tool":   cr.ToolName,
					"action": cr.Action,
					"detail": cr.Detail,
				})
			}
			if tracker.NeedsReviewCount() > 0 {
				s.logger.Log("EventA3NeedsReview", taskID, step.ID, map[string]any{
					"needs_review_count": tracker.NeedsReviewCount(),
				})
			}
		}
	}

	s.Mu.Lock()
	step.Status = schemas.StepBlocked
	s.Mu.Unlock()
	// [EVAL-OBS] 2026-10-07: blocked is the direct upstream of decision arbitration, must see root cause classification.
	// Migrated to Registry.System.FeatureFlags (hot-reloadable).
	if s.registry.System.FeatureFlags.EvalObs {
		fmt.Fprintf(os.Stderr, "[EVAL-OBS] BLOCKED task=%s step=%s cause=%s conf=%.2f spawnDepth=%d maxSpawn=%d suggestion=%q\n",
			taskID, step.ID, string(cause), conf, step.SpawnDepth, s.registry.System.MaxSpawnDepth, suggestion)
	}
	// E4 fix: enrich step_failed payload with role_id, suggestion, confidence.
	// All values are already in scope; this is purely additive — the thin
	// {blocked, root_cause} payload made external observability impossible.
	s.publishEvent(taskID, step.ID, EventStepFailed, map[string]any{
		"root_cause": string(cause),
		"blocked":    true,
		"role_id":    step.RoleID,
		"suggestion": suggestion,
		"confidence": conf,
	})

	if s.SignalField != nil {
		s.SignalField.Deposit(types.LayerCritical, step.ID, 1.5)
	}

	options := []string{"skip", "abort", "retry", "refine_and_retry"}
	if cause == FailureClassCapabilityRequired {
		ctx := s.lifecycleCtx
		if s.expStore != nil {
			for _, cap := range details {
				recommended := s.expStore.QuerySkillRecommendations(ctx, cap, 0.0)
				if len(recommended) > 0 {
					best := s.expStore.SelectSkill(ctx, recommended, cap, step.RoleID, "", 0.1)
					if best != "" {
						options = append(options, "retry_with_skill:"+best)
					}
				}
				options = append(options, "retry_with_skill:"+cap)
			}
		} else {
			for _, cap := range details {
				options = append(options, "retry_with_skill:"+cap)
			}
		}
	}
	if cause == FailureClassContextDeficit {
		options = append(options, "retry_with_context")
	}

	s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"root_cause":       string(cause),
		"suggested_action": suggestion,
		"missing_details":  details,
		"confidence":       conf,
		"missing_context":  output.MissingContext,
	}, options)

	return VerdictContinue
}
