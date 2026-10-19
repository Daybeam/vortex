package schemas

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/daybeam/vortex/pkg/jsonrepair"
)

// PlanDAG is the structured contract a Task Planner must emit (Path B of
// the two-tier planning model: "a dedicated planner step that produces the
// downstream DAG"). It is deliberately minimal — the planner selects
// role/app/task/deps, and names the exact tools each step may use.
//
// See docs/plan/ 52-planner-wiring-design.md in the eval kit for the
// rationale, and config/seed_builtins.go taskPlannerInstruction for the
// prompt that produces it.
type PlanDAG struct {
	Steps []PlanStep `json:"steps"`
}

// PlanStep is one node of the planner-produced DAG.
//
// Tool selection is EXPLICIT: the planner names allowed_tools rather than
// the engine deriving them from an app prefix. Rationale (user decision,
// 2026-10-18): a prefix derivation hands every login/logout/get_current_date
// tool to every step of that app, which is exactly the noise the router was
// already drowning in; naming tools keeps the blast radius readable. The
// parser therefore VALIDATES every name against the live catalog instead of
// guessing — an unknown name is a hard error, forcing the planner to retry
// rather than silently shipping a step that cannot log in.
type PlanStep struct {
	ID           string   `json:"id"`
	RoleID       string   `json:"role_id"`
	Task         string   `json:"task"`
	DependsOn    []string `json:"depends_on,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	ExitCriteria string   `json:"exit_criteria,omitempty"`
	// MCPID is the MCP server the AllowedTools belong to. Optional: when
	// empty the parser uses singleMCP (the common case — one bound server).
	MCPID string `json:"mcp_id,omitempty"`
}

// PlanParseOptions controls validation. All fields are required unless noted.
type PlanParseOptions struct {
	// AllowedRoles is the set of role IDs the planner may assign.
	AllowedRoles map[string]bool
	// ToolCatalog maps MCP ID -> set of tool names currently discoverable.
	// Used to validate AllowedTools. Never used to guess tools.
	ToolCatalog map[string]map[string]bool
	// SingleMCP is used when a PlanStep omits mcp_id. Empty means the caller
	// must set mcp_id on every step.
	SingleMCP string
	// MaxSteps bounds the produced DAG (0 => 12).
	MaxSteps int
	// MaxTaskChars bounds a single step description (0 => 4000).
	MaxTaskChars int
}

// PlanParseError distinguishes "the model produced something we will not
// execute" from genuine I/O failures, so callers can log a helpful reason
// instead of a generic wrap.
type PlanParseError struct {
	Reason string
	Detail string
}

func (e *PlanParseError) Error() string {
	if e.Detail == "" {
		return "planner output rejected: " + e.Reason
	}
	return fmt.Sprintf("planner output rejected: %s (%s)", e.Reason, e.Detail)
}

var planStepIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ParsePlanDAG turns raw planner output into validated StepInputs.
//
// Tolerances (learned from real LLM output):
//   - markdown code fences around the JSON
//   - truncated JSON (repaired via pkg/jsonrepair, Ring 1 primitive)
//   - a bare array instead of {"steps": [...]}
//
// Anything that cannot be validated is an error — the caller falls back to
// the original single-step submission. We never "best effort" a partial DAG:
// a step that silently lost its allowed_tools would fail at run time with a
// confusing tool-not-found, which is worse than not planning at all.
func ParsePlanDAG(raw string, opt PlanParseOptions) ([]StepInput, error) {
	maxSteps := opt.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 12
	}
	maxTask := opt.MaxTaskChars
	if maxTask <= 0 {
		maxTask = 4000
	}

	body, err := extractJSONObject(raw)
	if err != nil {
		return nil, err
	}

	var dag PlanDAG
	if err := json.Unmarshal([]byte(body), &dag); err != nil {
		return nil, &PlanParseError{Reason: "invalid JSON", Detail: err.Error()}
	}

	if len(dag.Steps) == 0 {
		return nil, &PlanParseError{Reason: "plan contains no steps"}
	}
	if len(dag.Steps) > maxSteps {
		return nil, &PlanParseError{
			Reason: "plan exceeds step budget",
			Detail: fmt.Sprintf("%d > %d", len(dag.Steps), maxSteps),
		}
	}

	seen := make(map[string]bool, len(dag.Steps))
	for i := range dag.Steps {
		ps := &dag.Steps[i]
		ps.ID = strings.TrimSpace(ps.ID)
		ps.RoleID = strings.TrimSpace(ps.RoleID)
		ps.Task = strings.TrimSpace(ps.Task)
		ps.MCPID = strings.TrimSpace(ps.MCPID)

		if ps.ID == "" {
			return nil, &PlanParseError{Reason: "step missing id", Detail: fmt.Sprintf("index %d", i)}
		}
		if !planStepIDPattern.MatchString(ps.ID) {
			return nil, &PlanParseError{Reason: "step id has illegal characters", Detail: ps.ID}
		}
		if seen[ps.ID] {
			return nil, &PlanParseError{Reason: "duplicate step id", Detail: ps.ID}
		}
		seen[ps.ID] = true

		if ps.RoleID == "" {
			return nil, &PlanParseError{Reason: "step missing role_id", Detail: ps.ID}
		}
		if len(opt.AllowedRoles) > 0 && !opt.AllowedRoles[ps.RoleID] {
			return nil, &PlanParseError{Reason: "unknown role_id", Detail: ps.RoleID}
		}

		if ps.Task == "" {
			return nil, &PlanParseError{Reason: "step missing task", Detail: ps.ID}
		}
		if len(ps.Task) > maxTask {
			return nil, &PlanParseError{
				Reason: "step task too long",
				Detail: fmt.Sprintf("%s: %d > %d chars", ps.ID, len(ps.Task), maxTask),
			}
		}
	}

	// Dependency integrity: every reference resolves, then acyclicity.
	for i := range dag.Steps {
		ps := &dag.Steps[i]
		for _, dep := range ps.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				return nil, &PlanParseError{Reason: "empty dependency reference", Detail: ps.ID}
			}
			if !seen[dep] {
				return nil, &PlanParseError{
					Reason: "dependency references unknown step", Detail: ps.ID + " -> " + dep,
				}
			}
			if dep == ps.ID {
				return nil, &PlanParseError{Reason: "step depends on itself", Detail: ps.ID}
			}
		}
	}
	if cycle := findPlanCycle(dag.Steps); cycle != "" {
		return nil, &PlanParseError{Reason: "cyclic dependency", Detail: cycle}
	}

	// Tool validation + materialisation.
	out := make([]StepInput, 0, len(dag.Steps))
	for i := range dag.Steps {
		ps := &dag.Steps[i]

		mcpID := ps.MCPID
		if mcpID == "" {
			mcpID = opt.SingleMCP
		}
		if mcpID == "" && len(ps.AllowedTools) > 0 {
			return nil, &PlanParseError{Reason: "allowed_tools given without mcp_id", Detail: ps.ID}
		}

		var allowlist map[string][]string
		if len(ps.AllowedTools) > 0 {
			// T7 fix (2026-10-18): when the tool catalog is missing (cold
			// engine — MCP handshake hasn't run yet), accept tools as-is
			// (unverified) instead of rejecting. The tools will be
			// intersected with discovered AvailableTools at execution time
			// (validator.go ResolveFinalMCPs). This allows planner_first to
			// produce accepted plans on a cold engine.
			//
			// When the catalog IS available, validation is mandatory: silently
			// dropping a planner-specified allowlist would widen privileges;
			// silently keeping an unvalidated one produces "tool not found" at
			// run time.
			catalog, hasCatalog := opt.ToolCatalog[mcpID]
			catalogAvailable := hasCatalog && len(catalog) > 0

			cleaned := make([]string, 0, len(ps.AllowedTools))
			var unknown []string
			for _, t := range ps.AllowedTools {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if catalogAvailable && !catalog[t] {
					unknown = append(unknown, t)
					continue
				}
				cleaned = append(cleaned, t)
			}
			if len(unknown) > 0 {
				sort.Strings(unknown)
				return nil, &PlanParseError{
					Reason: "unknown tool name(s)",
					Detail: ps.ID + ": " + strings.Join(unknown, ", "),
				}
			}
			if len(cleaned) == 0 {
				return nil, &PlanParseError{Reason: "allowed_tools empty after cleaning", Detail: ps.ID}
			}
			allowlist = map[string][]string{mcpID: cleaned}
		}

		inp := StepInput{
			ID:        ps.ID,
			RoleID:    ps.RoleID,
			Task:      ps.Task,
			DependsOn: append([]string(nil), ps.DependsOn...),
		}
		if mcpID != "" {
			inp.AdditionalMCPs = []string{mcpID}
		}
		if allowlist != nil {
			inp.AdditionalToolAllowlists = allowlist
		}
		if ec := strings.TrimSpace(ps.ExitCriteria); ec != "" {
			inp.ExitCriteria = ec
		}
		out = append(out, inp)
	}

	return out, nil
}

// extractJSONObject finds the outermost JSON object/array in raw model output,
// strips markdown fences, and repairs truncation. Returns the JSON body.
func extractJSONObject(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", &PlanParseError{Reason: "empty planner output"}
	}

	// Strip markdown code fences (```json ... ``` or ``` ... ```).
	if idx := strings.Index(s, "```"); idx >= 0 {
		rest := s[idx+3:]
		// Drop an optional language tag on the same line.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			firstLine := strings.TrimSpace(rest[:nl])
			if firstLine != "" && !strings.HasPrefix(firstLine, "{") && !strings.HasPrefix(firstLine, "[") {
				rest = rest[nl+1:]
			} else {
				rest = rest[nl+1:]
			}
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			rest = rest[:end]
		}
		s = strings.TrimSpace(rest)
	}

	// Tolerate a bare array: wrap it as {"steps": [...]}.
	if strings.HasPrefix(s, "[") {
		s = `{"steps":` + s + `}`
	}

	// Both jsonrepair paths are Ring 1 primitives; use the strict one first
	// (it only fires when the payload is already valid or trivially fixable),
	// then the truncation-tolerant one.
	if fixed, ok := jsonrepair.Repair(s); ok && fixed != "" {
		s = fixed
	} else if trimmed, ok := jsonrepair.TrimIncomplete(s); ok && trimmed != "" {
		if fixed2, ok2 := jsonrepair.Repair(trimmed); ok2 && fixed2 != "" {
			s = fixed2
		} else {
			s = trimmed
		}
	}

	if !json.Valid([]byte(s)) {
		return "", &PlanParseError{Reason: "could not recover valid JSON from planner output"}
	}
	if !strings.HasPrefix(s, "{") {
		return "", &PlanParseError{Reason: "planner output is not a JSON object"}
	}
	return s, nil
}

// findPlanCycle returns a human-readable cycle path ("a -> b -> a") or "".
// Iterative DFS; the step count is bounded by MaxSteps so recursion depth is
// not a concern either way.
func findPlanCycle(steps []PlanStep) string {
	deps := make(map[string][]string, len(steps))
	for _, s := range steps {
		deps[s.ID] = s.DependsOn
	}

	const (
		white = 0 // unvisited
		grey  = 1 // on stack
		black = 2 // done
	)
	color := make(map[string]int, len(steps))
	var stack []string

	var visit func(id string) string
	visit = func(id string) string {
		color[id] = grey
		stack = append(stack, id)
		for _, dep := range deps[id] {
			switch color[dep] {
			case grey:
				// Found a back edge — reconstruct the cycle from stack.
				start := 0
				for i, v := range stack {
					if v == dep {
						start = i
						break
					}
				}
				path := append(append([]string(nil), stack[start:]...), dep)
				return strings.Join(path, " -> ")
			case white:
				if c := visit(dep); c != "" {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return ""
	}

	// Deterministic iteration order keeps error messages reproducible.
	ids := make([]string, 0, len(steps))
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)

	for _, id := range ids {
		if color[id] == white {
			stack = stack[:0]
			if c := visit(id); c != "" {
				return c
			}
		}
	}
	return ""
}
