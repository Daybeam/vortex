package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type stepSuccessAction int

const (
	stepSuccessDone stepSuccessAction = iota
	stepSuccessRetry
	stepSuccessReturn
	stepSuccessExhausted
)

// finishSuccessfulStep runs Guardian, output handling, snapshots, dependency
// detection, context transition, and exit-criteria refinement in the original
// order. It reports how the retry loop should proceed.
func (s *DirectedEngine) finishSuccessfulStep(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult, stagedWS *StagedWorkspace, preSnapshot *SnapshotIndex) stepSuccessAction {
	taskID := graph.TaskID
	outputText := fmt.Sprintf("%v", result.Output.Result)
	if valid, reason := s.guardStepOutput(ctx, result, outputText); !valid {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error":      "Sieve Guardian intercepted",
			"reason":     reason,
			"root_cause": string(FailureClassContractViolation),
		})
		s.Mu.Lock()
		step.LastError = store.SanitizeError("sieve_intercepted: " + reason)
		step.PartialOutput = outputText
		step.RetryCount++
		s.Mu.Unlock()
		if step.RetryCount <= step.MaxRetries {
			return stepSuccessRetry
		}
		return stepSuccessExhausted
	}

	s.Mu.Lock()
	step.ModelID = result.ModelID
	s.Mu.Unlock()
	s.handleOutput(graph, step, result)

	var postSnapshot *SnapshotIndex
	if stagedWS != nil {
		if entry, post, serr := stagedWS.RecordPostStepSnapshot(taskID, step.ID, "completed", preSnapshot); serr != nil {
			s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "post_snapshot", "error": serr.Error()})
		} else {
			postSnapshot = post
			s.logger.Log("EventStagingSnapshotRecorded", taskID, step.ID, map[string]any{"diff_summary": entry.DiffSummary})
		}
	}
	if stepRes, terr := s.taskStore.Get(ctx, taskID, step.ID); terr == nil && stepRes != nil {
		if deps := s.detectReadDeps(graph, step.ID, stepRes.Trace, postSnapshot); len(deps) > 0 {
			step.ReadDeps = deps
		}
	}
	if s.JITSessions != nil {
		s.JITSessions.CloseSessionsForStep(taskID, step.ID)
	}
	s.transitionContextTree(graph, step, result)

	// O47 P1 FIX: require_audit=true or VerifierModel set must trigger the
	// gate even when ExitCriteria is empty.
	if step.Status == schemas.StepOK &&
		(step.ExitCriteria != "" || step.RequireAudit || step.VerifierModel != "") {
		if ok, reason, failType := s.verifyExitCriteria(ctx, graph, step, result); !ok {
			if step.AutoRefineCount < step.MaxAutoRefine {
				step.AutoRefineCount++
				s.Mu.Lock()
				step.Status = schemas.StepPending
				step.LastError = store.SanitizeError(fmt.Sprintf("[%s] Exit criteria not met: %s. Retrying...", failType, reason))
				s.Mu.Unlock()
				if ctx.Err() != nil {
					return stepSuccessReturn
				}
				s.logger.Log(obs.EventStepRetrying, taskID, step.ID, map[string]any{
					"reason": "exit_criteria_fail", "details": reason, "refine_attempt": step.AutoRefineCount, "fail_type": failType,
				})
				recoveryAdvice := exitCriteriaRecoveryAdvice(failType)
				step.Task = fmt.Sprintf("%s\n\n%s\nYour previous output failed verification.\nFailure Category: %s\nReason: %s\nAdvice: %s", step.Task, MarkerRefinementRequired, failType, reason, recoveryAdvice)
				return stepSuccessRetry
			}
			s.Mu.Lock()
			step.Status = schemas.StepBlocked
			s.Mu.Unlock()
			s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
				"error":  "Failed exit criteria after max refinements",
				"reason": reason,
			}, []string{"skip", "abort", "retry"})
		}
	}
	s.runInterceptors(step, result)
	s.persistGraph(graph)
	return stepSuccessDone
}

func exitCriteriaRecoveryAdvice(failType schemas.VerificationFailureType) string {
	switch failType {
	case schemas.FailurePermission:
		return "It seems like a permission issue. Please check if you need to use a different tool or request elevated access."
	case schemas.FailureTimeout:
		return "The operation timed out. Consider breaking the task into smaller sub-tasks, increasing efficiency, or using a more performant tool if available."
	case schemas.FailureConflict:
		return "A state conflict (e.g. file modified by another process) was detected. Please RE-READ the current state (file or UI) before attempting to write again to ensure your changes are based on the latest version."
	case schemas.FailureSchemaViolation:
		return "Your output violates the required schema. Please strictly follow the formatting instructions provided in the tool definition or task description."
	default:
		return "Your output failed logical or semantic verification. Please carefully review the 'Reason' above and adjust your approach accordingly."
	}
}

// finishExhaustedStep records failure, rolls back staged changes, closes JIT
// sessions, and blocks dependents after the retry loop is exhausted.
func (s *DirectedEngine) finishExhaustedStep(graph *schemas.TaskGraph, step *schemas.Step, stagedWS *StagedWorkspace, preSnapshot *SnapshotIndex) {
	taskID := graph.TaskID
	s.Mu.Lock()
	step.Status = schemas.StepFailed
	s.Mu.Unlock()
	s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
		"last_error": step.LastError,
		"root_cause": string(s.classifyErrorWithSystemOne(errors.New(step.LastError), step.ID, step.RoleID, step.Task)),
	})
	if stagedWS != nil {
		if _, _, serr := stagedWS.RecordPostStepSnapshot(taskID, step.ID, "failed", preSnapshot); serr != nil {
			s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "post_snapshot_failed", "error": serr.Error()})
		}
		if preSnapshot != nil {
			if rerr := stagedWS.Rollback(preSnapshot); rerr != nil {
				s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "rollback", "error": rerr.Error()})
			} else {
				if merr := stagedWS.MarkRolledBack(step.ID); merr != nil {
					s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "mark_rolled_back", "error": merr.Error()})
				}
				s.logger.Log("EventStagingRolledBack", taskID, step.ID, nil)
			}
		}
	}
	if s.JITSessions != nil {
		s.JITSessions.CloseSessionsForStep(taskID, step.ID)
	}
	s.maybeBlock(graph, step)
}
