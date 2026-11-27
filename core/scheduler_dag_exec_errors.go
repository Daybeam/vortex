package core

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type stepErrorAction int

const (
	stepErrorReturn stepErrorAction = iota
	stepErrorRetry
	stepErrorExhausted
)

// handleStepSpawnError preserves the original decision ordering and all
// terminal/retry side effects. The caller performs the corresponding loop
// control after the engine mutex has been released.
func (s *DirectedEngine) handleStepSpawnError(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, role *config.Role, err error) stepErrorAction {
	taskID := graph.TaskID
	if strings.Contains(err.Error(), "DYNAMIC_ESCALATION_REQUIRED") {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error":      "Dynamic Escalation Required",
			"reason":     err.Error(),
			"root_cause": string(s.classifyErrorWithSystemOne(err, step.ID, step.RoleID, step.Task)),
		})
		s.Mu.Lock()
		step.Status = schemas.StepBlocked
		s.Mu.Unlock()
		s.addDecision(graph, step, schemas.DecisionDelegationRequired, map[string]any{
			"kind":            DelegationKindDAGSurgery,
			"error":           "Context overflow: task too complex for single model.",
			"action_required": "Please split this step into smaller sub-tasks.",
		}, []string{"skip", "abort"})
		return stepErrorReturn
	}

	if strings.Contains(err.Error(), ErrMaxTurnsExhausted) {
		s.Mu.Lock()
		step.Status = schemas.StepBlocked
		step.LastError = err.Error()
		s.Mu.Unlock()
		s.logger.Log(obs.EventDecisionRequired, taskID, step.ID, map[string]any{
			"reason":      "max_tool_turns_exhausted",
			"error":       err.Error(),
			"retry_count": step.RetryCount,
			"resume_hint": "choice 'retry' re-spawns this step from scratch; choose 'refine_and_retry' to inject additional_prompt_context carrying the partial progress",
		})
		s.addDecision(graph, step, schemas.DecisionMaxTurnsExhausted, map[string]any{
			"error":           err.Error(),
			"retry_count":     step.RetryCount,
			"action_required": "Step exhausted its tool-turn budget before producing a final answer. Choose an option to proceed.",
			"partial_context": "Prior tool interactions are recorded in this step's trace. Use refine_and_retry with feedback to carry that context forward.",
		}, []string{"resume_more_turns", "refine_and_retry", "escalate_model", "retry", "skip", "abort"})
		return stepErrorReturn
	}

	var rl *providers.RateLimitError
	if errors.As(err, &rl) {
		wait := s.retryWait(role, step.RetryCount)
		usedServerHint := false
		if rl.RetryAfter > 0 {
			wait = rl.RetryAfter
			if wait > 300 {
				wait = 300
			}
			usedServerHint = true
		}
		s.logger.Log(obs.EventRateLimited, taskID, step.ID, map[string]any{
			"attempt": step.RetryCount, "wait_seconds": wait, "used_server_retry_after": usedServerHint,
			"provider_id": step.ProviderID,
		})
		if step.ProviderID != "" {
			s.registry.SetCooldown(step.ProviderID, time.Duration(wait)*time.Second)
		}
		s.Mu.Lock()
		step.RetryCount++
		s.Mu.Unlock()
		if step.RetryCount > step.MaxRetries {
			return stepErrorExhausted
		}
		if !waitForStepRetry(ctx, time.Duration(wait)*time.Second) {
			return stepErrorReturn
		}
		return stepErrorRetry
	}

	// A spent provider-request allowance cannot grow between attempts.
	var be *providers.BudgetExhaustedError
	if errors.As(err, &be) {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error":      "Provider request budget exhausted",
			"details":    be.Msg,
			"root_cause": string(FailureClassCostOverrun),
		})
		s.Mu.Lock()
		step.LastError = store.SanitizeError(be.Error())
		if step.TriggerError == "" {
			step.TriggerError = step.LastError
		}
		s.Mu.Unlock()
		return stepErrorExhausted
	}

	var br *providers.BadRequestError
	if errors.As(err, &br) {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error":      "Bad Request - triggering graceful degradation",
			"details":    br.Msg,
			"root_cause": string(FailureClassBadRequest),
		})
		s.Mu.Lock()
		step.Status = schemas.StepSkipped
		step.LastError = store.SanitizeError("degraded: " + br.Msg)
		s.Mu.Unlock()
		return stepErrorReturn
	}

	s.Mu.Lock()
	step.LastError = store.SanitizeError(err.Error())
	if step.TriggerError == "" {
		step.TriggerError = step.LastError
	}
	s.Mu.Unlock()

	if strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "PERMISSION_DENIED") || strings.Contains(err.Error(), "HTTP 401") {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error":      "Circuit Breaker triggered: Auth failure",
			"details":    err.Error(),
			"root_cause": string(FailureClassAuthPermission),
		})
		return stepErrorExhausted
	}
	if strings.Contains(err.Error(), "missing dependency:") {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error":      "Environment check failed",
			"details":    err.Error(),
			"root_cause": string(FailureClassMissingDependency),
		})
		s.Mu.Lock()
		step.Status = schemas.StepBlocked
		s.Mu.Unlock()
		if s.applyStepFailurePolicy(graph, step, taskID, FailureClassMissingDependency) {
			return stepErrorReturn
		}
		s.addDecision(graph, step, schemas.DecisionEnvironmentMissing, map[string]any{
			"missing_dependency": err.Error(),
		}, []string{"skip", "abort", "retry_with_skill:shell"})
		return stepErrorReturn
	}
	if strings.Contains(err.Error(), "role \"") && (strings.Contains(err.Error(), "\" not found") || strings.Contains(err.Error(), "generation failed") || strings.Contains(err.Error(), "generation is disabled")) {
		s.logger.Log(obs.EventRoleMissing, taskID, step.ID, map[string]any{
			"error": err.Error(), "role": step.RoleID,
		})
		s.setStepAndGraphStatus(step, schemas.StepBlocked, graph, schemas.GraphRunning)
		if s.SignalField != nil {
			s.SignalField.Deposit(types.LayerCritical, step.ID, 2.0)
		}
		if s.applyStepFailurePolicy(graph, step, taskID, FailureClassRoleMissing) {
			s.persistGraph(graph)
			return stepErrorReturn
		}
		s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
			"error":           "Role missing: " + err.Error(),
			"role_id":         step.RoleID,
			"action_required": "Choose create_role to synthesise a task-scoped role from the cookbook, or register one globally via orchestrator_invoke(subsystem=\"config\", action=\"register_role\").",
		}, []string{"create_role", "retry", "skip", "abort"})
		s.persistGraph(graph)
		return stepErrorReturn
	}
	if strings.Contains(err.Error(), "skill \"") && strings.Contains(err.Error(), "\" not found") {
		s.logger.Log(obs.EventStepFailed, taskID, step.ID, map[string]any{
			"error": err.Error(), "role": step.RoleID, "root_cause": string(FailureClassRoleMissing),
		})
		s.setStepAndGraphStatus(step, schemas.StepBlocked, graph, schemas.GraphRunning)
		s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
			"error":           "Infrastructure failure: " + err.Error(),
			"action_required": "Please register the missing skill or remove it from the role's bound_skills",
		}, []string{"create_skill", "retry", "skip", "abort"})
		s.persistGraph(graph)
		return stepErrorReturn
	}

	s.Mu.Lock()
	step.RetryCount++
	s.Mu.Unlock()
	if ctx.Err() != nil {
		return stepErrorReturn
	}
	s.logger.Log(obs.EventStepRetrying, taskID, step.ID, map[string]any{
		"attempt": step.RetryCount, "error": err.Error(),
	})
	if step.RetryCount > step.MaxRetries {
		return stepErrorExhausted
	}
	wait := time.Duration(math.Pow(2, float64(step.RetryCount))) * time.Second
	if !waitForStepRetry(ctx, wait) {
		return stepErrorReturn
	}
	return stepErrorRetry
}
