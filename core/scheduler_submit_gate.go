package core

import (
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// validateSubmission runs the submission-time guards in one place: the task
// complexity/relevance pre-check plus the role-callability gate (AUTH-1).
func (s *DirectedEngine) validateSubmission(inputs []schemas.StepInput) error {
	if err := s.validateTaskComplexity(inputs); err != nil {
		return err
	}
	return s.roleCallableBySubmitter(inputs)
}

// roleCallableBySubmitter enforces AUTH-1 (2026-10-23): a step's role_id must
// be callable by the submitter's tier. The mount gate bounds what a named role
// can REACH; this bounds WHICH role a non-admin caller may name. Empty
// Role.CallableTiers = open (pre-existing behavior); the meta roles
// (task_planner / decision_arbiter / sop_generator) declare ["admin"].
//
// Runs at the engine chokepoint (called from SubmitWithSessionIR via
// validateSubmission) so every entry point — MCP tool, HTTP, group dispatcher,
// schedule — is covered. A caller that fails to set SubmitterIsAdmin is treated
// as non-admin (fail-closed).
//
// Note: this checks the CALLER's inputs, before the planner may replace them.
// Sufficient because the planner's assignable-role list already excludes the
// meta roles (scheduler_planner.go plannerAllowedRoles).
func (s *DirectedEngine) roleCallableBySubmitter(inputs []schemas.StepInput) error {
	if s.registry == nil {
		return nil
	}
	for i := range inputs {
		if inputs[i].SubmitterIsAdmin || inputs[i].RoleID == "" {
			continue
		}
		s.registry.Mu.RLock()
		role := s.registry.Roles[inputs[i].RoleID]
		s.registry.Mu.RUnlock()
		if role != nil && !role.CallableByAdmin(false) {
			return fmt.Errorf("role %q is not callable by a non-admin submitter", inputs[i].RoleID)
		}
	}
	return nil
}
