package core

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_failure.go — Phase 3: Step failure/partial handling.
// Extracted from handleOutput per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md Step 3.
// Returns VerdictReturn when an early-exit path (backtrack, ASAE, fallback)
// handles the situation; VerdictContinue when it falls through to decision
// blocking.

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

	fallback := graph.FallbackFor(step.ID)
	if fallback != nil && fallback.Status == schemas.StepPending {
		s.Mu.Lock()
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

	s.Mu.Lock()
	step.Status = schemas.StepBlocked
	s.Mu.Unlock()
	s.publishEvent(taskID, step.ID, EventStepFailed, map[string]any{"root_cause": string(cause), "blocked": true})

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
	options = append(options, "escalate_model")

	s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"root_cause":       string(cause),
		"suggested_action": suggestion,
		"missing_details":  details,
		"confidence":       conf,
		"missing_context":  output.MissingContext,
	}, options)

	return VerdictContinue
}
