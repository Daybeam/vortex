package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

func (s *Sieve) SetPlan(taskID, plan string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plans == nil {
		s.plans = make(map[string]string)
	}
	s.plans[taskID] = plan
}

// ClearPlan removes the plan for a task (call on step completion).
func (s *Sieve) ClearPlan(taskID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.plans, taskID)
}

// LoadPlan returns the stored plan for a task, or "" if none.
func (s *Sieve) LoadPlan(taskID string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plans[taskID]
}

// DetectPlanDrift checks whether a single tool call deviates from the
// stored step plan. Returns a non-empty steer message if drift is
// detected, or "" if the call is consistent with the plan (or no plan
// exists).
//
// Heuristic: if the tool is a mutating tool (write/edit/exec/commit/
// delete/move/create) and neither the tool name nor its primary argument
// appears in the plan text, the call is likely out of scope.
//
// This is intentionally conservative — read tools never trigger drift
// (reading is always safe), and mutating tools only trigger if neither
// the tool name nor any argument value appears in the plan.
func (s *Sieve) DetectPlanDrift(taskID string, tc schemas.ToolCall) string {
	if s == nil {
		return ""
	}
	plan := s.LoadPlan(taskID)
	if plan == "" {
		return "" // no plan → no drift detection
	}

	toolName := strings.ToLower(tc.Name)

	// Read-only tools never drift — reading is always within scope.
	if isReadTool(toolName) || isPollerTool(toolName) {
		return ""
	}

	// Check if the tool name appears in the plan.
	if strings.Contains(strings.ToLower(plan), toolName) {
		return "" // tool name mentioned in plan → likely in scope
	}

	// Check if any argument value appears in the plan.
	for _, v := range tc.Arguments {
		if str, ok := v.(string); ok && str != "" {
			if strings.Contains(strings.ToLower(plan), strings.ToLower(str)) {
				return "" // argument value mentioned in plan → likely in scope
			}
		}
	}

	// Mutating tool with no mention in plan → drift.
	return fmt.Sprintf(
		"[PLAN DRIFT] Tool %q was called but is not mentioned in the step plan. "+
			"Review the plan and ensure this action is necessary. If it is, "+
			"continue; if not, return to the planned steps.",
		tc.Name,
	)
}
