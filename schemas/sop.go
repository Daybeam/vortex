package schemas

import "encoding/json"

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
	ID            string   `json:"id"`
	Role          string   `json:"role"`
	Task          string   `json:"task"`
	Skills        []string `json:"skills,omitempty"`
	MCPs          []string `json:"mcps,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	DependsOn     []string `json:"depends_on,omitempty"`
	ExitCriteria  string   `json:"exit_criteria,omitempty"`
	ConditionExpr string   `json:"condition_expr,omitempty"` // Optional branching expression
	OnSuccess     string   `json:"on_success,omitempty"`     // Explicit success jump
	OnFailure     string   `json:"on_failure,omitempty"`     // Explicit failure jump (fallback)
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
			json.Unmarshal(b, &s.Triggers)
		case map[string]any:
			var t SOPTrigger
			b, _ := json.Marshal(v)
			json.Unmarshal(b, &t)
			s.Triggers = []SOPTrigger{t}
		}
	}

	// Handle Steps: map or slice
	if aux.Steps != nil {
		switch v := aux.Steps.(type) {
		case map[string]any:
			b, _ := json.Marshal(v)
			json.Unmarshal(b, &s.Steps)
		case []any:
			s.Steps = make(map[string]SOPStep)
			for _, item := range v {
				var step SOPStep
				b, _ := json.Marshal(item)
				if err := json.Unmarshal(b, &step); err == nil && step.ID != "" {
					s.Steps[step.ID] = step
				}
			}
		}
	}

	return nil
}
