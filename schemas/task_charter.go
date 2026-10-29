package schemas

import (
	"strings"
	"time"
)

// TaskCharter captures the task's intent (why), non-goals (what not to do),
// and constraints (what must hold). It is the task-level complement to
// ExitCriteria: ExitCriteria defines "done", Charter defines "on-track".
//
// All fields are optional — absence means "unspecified" and behavior is
// identical to pre-charter tasks (backward compatible).
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P1.
type TaskCharter struct {
	Intent      string   `json:"intent,omitempty"`        // why this task exists, 1-3 sentences
	NonGoals    []string `json:"non_goals,omitempty"`     // explicitly out of scope
	Constraints []string `json:"constraints,omitempty"`   // invariants that must hold
	Version     int      `json:"version,omitempty"`       // incremented on each approved change
}

// CharterPrompt formats the charter for injection into a step's system prompt.
// Returns empty string when no charter is set (backward compatible).
// Total output is capped at maxChars; excess is truncated with a marker.
func (c *TaskCharter) CharterPrompt(maxChars int) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Task Charter\n")
	if c.Intent != "" {
		sb.WriteString("**Intent:** ")
		sb.WriteString(c.Intent)
		sb.WriteString("\n")
	}
	if len(c.NonGoals) > 0 {
		sb.WriteString("**Non-Goals (do NOT do these):**\n")
		for _, ng := range c.NonGoals {
			sb.WriteString("- ")
			sb.WriteString(ng)
			sb.WriteString("\n")
		}
	}
	if len(c.Constraints) > 0 {
		sb.WriteString("**Constraints (must hold):**\n")
		for _, cs := range c.Constraints {
			sb.WriteString("- ")
			sb.WriteString(cs)
			sb.WriteString("\n")
		}
	}
	result := sb.String()
	if maxChars > 0 && len(result) > maxChars {
		result = result[:maxChars] + "\n... (charter truncated)"
	}
	return result
}

// Substitution records a single instance where the actual execution differed
// from what was requested (e.g. role fallback, MCP degradation). The ledger
// is appended to TaskGraph.Substitutions and surfaced in get_task_status so
// that task drift is visible without digging through logs.
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P3.
type Substitution struct {
	Kind      string    `json:"kind"`                // role_fallback | ephemeral_role | mcp_degraded | fallback_step | model_escalation
	Requested string    `json:"requested,omitempty"`  // what was originally requested
	Actual    string    `json:"actual,omitempty"`     // what was actually used
	Reason    string    `json:"reason,omitempty"`     // why the substitution happened
	StepID    string    `json:"step_id,omitempty"`
	At        time.Time `json:"at"`
}
