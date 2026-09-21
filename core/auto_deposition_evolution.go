package core

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// DepositExperienceEvolution captures a successful recovery from a failure
// and persists it as an Anti-Pattern precedent. ADDED (2026-09-08) for
// Decision-Driven Experience Evolution.
func (s *DirectedEngine) DepositExperienceEvolution(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step) {
	if s.expStore == nil || step.TriggerError == "" || step.AdditionalPromptContext == "" {
		return
	}

	failurePattern := step.TriggerError
	recoveryAction := step.AdditionalPromptContext

	// Derive trigger condition from step ID and role
	trigger := fmt.Sprintf("%s (%s)", step.ID, step.RoleID)

	// Type-assert to *store.ExperienceStore to access AntiPatternStore
	concreteES, ok := s.expStore.(*store.ExperienceStore)
	if !ok || concreteES.AntiPatternStore == nil {
		return
	}

	intent := step.Task
	if len(intent) > 100 {
		intent = intent[:100]
	}

	s.logger.Log("EventExperienceEvolution", graph.TaskID, step.ID, map[string]any{
		"action":          "deposit_anti_pattern",
		"failure_mode":    failurePattern,
		"recovery_action": recoveryAction,
	})

	concreteES.AntiPatternStore.AutoDepositDecisionRecovery(
		ctx,
		intent,
		fmt.Sprintf("Pitfall in %s", trigger), // AntiPattern field
		fmt.Sprintf("Task: %s", intent),       // TriggerCondition field
		failurePattern,                        // Symptom field
		recoveryAction,                        // CorrectPattern field
	)
}
