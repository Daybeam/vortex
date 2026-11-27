package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// parsePlannerOutput validates raw planner text into StepInputs using the
// same contract the submission path uses, so the two entry points cannot
// drift apart.
func (s *DirectedEngine) parsePlannerOutput(raw, targetRole string, submitterIsAdmin bool) ([]schemas.StepInput, error) {
	_, catalog := s.plannerToolCatalog(targetRole, submitterIsAdmin)
	opt := s.plannerParseOptions(targetRole, catalog)
	return schemas.ParsePlanDAG(raw, opt)
}

// plannerResultText extracts the model's text from a SpawnResult regardless
// of which of the several shapes the subagent returned.
//
// Why the "steps" case exists (2026-10-18): a planner that answers with the
// bare contract object {"steps": [...]} — exactly what planJSONContract asks
// for — is parsed by schemas.ParseOutput into Result["steps"], not
// Result["text"], because the object has no "result"/"confidence" fields to
// populate SubagentOutput. The old fallback then printed the Go map with
// fmt.Sprintf("%v") ("map[steps:[map[allowed_tools:[...]]]]"), which is not
// JSON, so ParsePlanDAG rejected a perfectly valid plan with "could not
// recover valid JSON". Observed with google/gemini-3-flash-preview on
// 4fab96f_3 / the 2+2 probe. Re-serialising with encoding/json keeps the
// structure intact and lets extractJSONObject do its job.
func plannerResultText(res *SpawnResult) string {
	if res == nil {
		return ""
	}
	if s, ok := res.Output.Result["text"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := res.Output.Result["reply"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	for _, k := range []string{"result", "output", "content", "answer"} {
		if s, ok := res.Output.Result[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	// A structured plan that skipped the subagent output wrapper. Marshal it
	// back to JSON rather than to Go-syntax `map[...]`.
	if v, ok := res.Output.Result["steps"]; ok {
		if b, err := json.Marshal(map[string]any{"steps": v}); err == nil {
			return string(b)
		}
	}
	// Last resort: the whole result. Marshal as JSON first (still parseable
	// when it contains the plan); only then fall back to Go-syntax printing,
	// which ParsePlanDAG will reject — correctly, since it is not JSON.
	if len(res.Output.Result) > 0 {
		if b, err := json.Marshal(res.Output.Result); err == nil && json.Valid(b) {
			if s := string(b); strings.TrimSpace(s) != "{}" && strings.TrimSpace(s) != "null" {
				return truncateForLog(s, 20000)
			}
		}
	}
	return truncateForLog(fmt.Sprintf("%v", res.Output.Result), 20000)
}
