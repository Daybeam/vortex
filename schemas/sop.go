package schemas

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SOP represents a Standard Operating Procedure workflow template.
type SOP struct {
	ID             string             `json:"id"`
	Version        string             `json:"version"`
	Author         string             `json:"author"`           // e.g. "human_expert", "system"
	MutableByAgent bool               `json:"mutable_by_agent"` // false for human-authored baseline
	Description    string             `json:"description"`
	Tags           []string           `json:"tags,omitempty"`
	TypicalTasks   []string           `json:"typical_tasks,omitempty"`
	Triggers       []SOPTrigger       `json:"triggers"`
	Steps          map[string]SOPStep `json:"steps"`
	Metadata       map[string]any     `json:"metadata,omitempty"`
}

// SOPTrigger defines when this SOP should be automatically matched.
type SOPTrigger struct {
	Keywords []string `json:"keywords"`
}

// SOPStep defines a node in the workflow DAG.
type SOPStep struct {
	ID        string   `json:"id"`
	Role      string   `json:"role"`
	Task      string   `json:"task"`
	Skills    []string `json:"skills,omitempty"`
	MCPs      []string `json:"mcps,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	// ExitCriteria is the step's gate. Only declarative forms are legal since
	// Q2-4 (2026-11-06): "regex:<pattern>", "file:<path>", or "json"; empty
	// means no gate. Prose is still substring-matched at runtime for legacy
	// files, but it is not a contract and SOP load warns about it.
	ExitCriteria string `json:"exit_criteria,omitempty"`
	// DeterministicChecks mirrors Step.DeterministicChecks; absent here it is dropped at parse time (O1).
	DeterministicChecks []DeterministicCheck `json:"deterministic_checks,omitempty"`
	ConditionExpr       string               `json:"condition_expr,omitempty"` // Optional branching expression
	OnSuccess           string               `json:"on_success,omitempty"`     // Explicit success jump
	OnFailure           string               `json:"on_failure,omitempty"`     // Explicit failure jump (fallback)
}

// UnmarshalJSON implements custom unmarshaling for SOP to support legacy formats
// where Triggers was a single object and Steps was a slice.
func (s *SOP) UnmarshalJSON(data []byte) error {
	type Alias SOP
	aux := &struct {
		Triggers any `json:"triggers"`
		Steps    any `json:"steps"`
		*Alias
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Handle Triggers: slice or single object
	if aux.Triggers != nil {
		switch v := aux.Triggers.(type) {
		case []any:
			b, _ := json.Marshal(v)
			// audit L-15-3: return error instead of silently dropping triggers.
			if err := json.Unmarshal(b, &s.Triggers); err != nil {
				return fmt.Errorf("parse SOP triggers: %w", err)
			}
		case map[string]any:
			var t SOPTrigger
			b, _ := json.Marshal(v)
			if err := json.Unmarshal(b, &t); err != nil {
				return fmt.Errorf("parse SOP trigger: %w", err)
			}
			s.Triggers = []SOPTrigger{t}
		}
	}

	// Handle Steps: map or slice
	if aux.Steps != nil {
		switch v := aux.Steps.(type) {
		case map[string]any:
			b, _ := json.Marshal(v)
			// audit L-15-3: return error instead of silently dropping steps.
			if err := json.Unmarshal(b, &s.Steps); err != nil {
				return fmt.Errorf("parse SOP steps: %w", err)
			}
		case []any:
			s.Steps = make(map[string]SOPStep)
			for _, item := range v {
				var step SOPStep
				b, _ := json.Marshal(item)
				// audit L-16-1: return error instead of silently skipping malformed entries.
				if err := json.Unmarshal(b, &step); err != nil {
					return fmt.Errorf("parse SOP steps: %w", err)
				}
				if step.ID == "" {
					return fmt.Errorf("parse SOP steps: step missing ID")
				}
				s.Steps[step.ID] = step
			}
		}
	}

	return nil
}

// ExitCriteriaIsDeclarative reports whether c is one of the legal SOP
// exit_criteria forms (Q2-4, 2026-11-06): "regex:<pattern>", "file:<path>",
// or "json". Empty means "no gate" and is legal.
//
// A prefix with an empty payload is not legal: "regex:" compiles to a pattern
// that matches everything and "file:" can never stat a path, so both are
// degenerate gates that would silently pass or never pass.
func ExitCriteriaIsDeclarative(c string) bool {
	c = strings.TrimSpace(c)
	switch {
	case c == "", c == "json":
		return true
	case strings.HasPrefix(c, "regex:"):
		return strings.TrimSpace(strings.TrimPrefix(c, "regex:")) != ""
	case strings.HasPrefix(c, "file:"):
		return strings.TrimSpace(strings.TrimPrefix(c, "file:")) != ""
	default:
		return false
	}
}
