package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// ─── Task-level Planner: shared plumbing ──────────────────────────────────
//
// The engine has two planning tiers:
//
//	Tier 1 (task level, "who builds the DAG"):
//	   a) the session main agent submits an explicit steps[]  → orchestrator_submit_task
//	   b) a weak main agent delegates to the builtin `task_planner` role  → this file
//	Tier 2 (step level, "how one step sequences its own work"):
//	   core/spawner_plan.go runPlannerPhase — markdown only, does NOT create steps.
//
// This file only serves tier 1(b). It intentionally does not touch tier 2.
//
// The tier 1(b) path was previously unwired: `task_planner` existed as a
// builtin role, `MutateGraphTopology` existed as a DAG surgery primitive, but
// nothing connected "planner output" to "graph injection". See
// docs/plan 52-planner-wiring-design.md in the eval kit.

// planJSONContract is the exact output contract appended to every planner
// prompt. It must stay in sync with schemas.PlanDAG / schemas.PlanStep.
const planJSONContract = `
## Output Contract (MANDATORY)

Respond with ONE JSON object and nothing else. No prose, no markdown fences.

{
  "steps": [
    {
      "id": "<unique id, ^[A-Za-z0-9_-]+$>",
      "role_id": "<one of the allowed role ids listed above>",
      "task": "<self-contained instruction for this step>",
      "depends_on": ["<id of a predecessor step>", ...],
      "allowed_tools": ["<exact tool name from the catalog below>", ...],
      "exit_criteria": "<optional: 'regex:<pattern>' or 'file:<path>' or empty>"
    }
  ]
}

Hard rules — violating any of these makes the plan unusable and it will be discarded:
1. Every "role_id" MUST appear in the allowed roles list.
2. Every "allowed_tools" entry MUST be copied verbatim from the tool catalog.
   Do not invent, abbreviate, or translate tool names.
3. Every "depends_on" entry MUST reference an "id" defined in this same plan.
4. The dependency graph MUST be acyclic.
5. Steps with no dependency between them will run in parallel. Use this to
   express fan-out; use "depends_on" to express join.
6. Emit only work that is still pending. Never re-emit an already completed step.
7. "allowed_tools" is the ONLY set of tools a step may call. If a step needs to
   authenticate first, the login tool must be listed too.`

// plannerToolCatalog renders the tool catalog a planner is allowed to choose
// from, grouped by MCP. Only tools that are actually discoverable are listed,
// so a planner can never name a tool that will not exist at run time.
//
// Returns the rendered text plus the machine-readable catalog used by
// schemas.ParsePlanDAG for validation.
//
// The second return value's emptiness is load-bearing: ParsePlanDAG treats an
// absent catalog for a named mcp_id as "tool catalog unavailable" and REJECTS
// the plan. That is the intended failure — a planner with no catalog can only
// invent tool names.
//
// 🔴 KNOWN LIMITATION (2026-10-18, measured): during RequestPlanning the
// engine has not yet completed an MCP handshake for this task, so
// config.MCPDef.AvailableTools is EMPTY for every MCP. Discovery happens
// inside spawner.buildMCPServers (spawner_mcp.go:223) while serving a step;
// RequestPlanning runs BEFORE the first step exists, so on a cold engine the
// catalog is always empty. Consequence: planner_first cannot produce an
// accepted plan on AppWorld — it is rejected at "tool catalog unavailable"
// (or emits invented names like "venmo.list_payment_requests" instead of
// "venmo__list_payment_requests" when allowed_tools is omitted entirely and
// SingleMCP lets the empty catalog slip past validation).
//
// The fix has to be either (a) pre-discovering tools before planning, or
// (b) a config-declared static tool catalog for planner use. Both were
// deliberately deferred: (a) doubles MCP startup cost per submission and
// (b) duplicates a 60-entry list per MCP that will silently rot. Until one
// lands, keep enable_planner_first=false for AppWorld.
func (s *DirectedEngine) plannerToolCatalog(roleID string) (string, map[string]map[string]bool) {
	catalog := map[string]map[string]bool{}
	if s.registry == nil {
		return "", catalog
	}

	// Prefer the role's bound MCPs — the planner is planning *for* that role,
	// so exposing unrelated servers would invite invalid assignments.
	var mcpIDs []string
	s.registry.Mu.RLock()
	if role, ok := s.registry.Roles[roleID]; ok && role != nil {
		for _, b := range role.BoundMCPBindings {
			if b.MCPID != "" {
				mcpIDs = append(mcpIDs, b.MCPID)
			}
		}
	}
	if len(mcpIDs) == 0 {
		// No binding declared: fall back to every configured MCP. This keeps
		// generic (non-AppWorld) deployments working, where roles often rely
		// on dynamic MCP assignment.
		for id := range s.registry.MCPs {
			mcpIDs = append(mcpIDs, id)
		}
		for id := range s.registry.DynamicMCPs {
			mcpIDs = append(mcpIDs, id)
		}
	}
	s.registry.Mu.RUnlock()

	sort.Strings(mcpIDs)

	var sb strings.Builder
	for _, id := range mcpIDs {
		s.registry.Mu.RLock()
		mcpDef, ok := s.registry.MCPs[id]
		if !ok {
			mcpDef, ok = s.registry.DynamicMCPs[id]
		}
		s.registry.Mu.RUnlock()
		if !ok || mcpDef == nil {
			continue
		}

		names := append([]string(nil), mcpDef.AvailableTools...)
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)

		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		catalog[id] = set

		desc := make(map[string]string, len(mcpDef.FullToolDefinitions))
		for _, d := range mcpDef.FullToolDefinitions {
			desc[d.Name] = d.Description
		}

		sb.WriteString(fmt.Sprintf("\n### MCP `%s` (%d tools)\n", id, len(names)))
		for _, n := range names {
			if d := strings.TrimSpace(desc[n]); d != "" {
				sb.WriteString(fmt.Sprintf("- `%s` — %s\n", n, firstLine(d, 160)))
			} else {
				sb.WriteString(fmt.Sprintf("- `%s`\n", n))
			}
		}
	}

	return sb.String(), catalog
}

// plannerAllowedRoles renders + returns the role ids a plan may assign.
// Only roles that exist in the registry are offered.
func (s *DirectedEngine) plannerAllowedRoles() (string, map[string]bool) {
	allowed := map[string]bool{}
	if s.registry == nil {
		return "", allowed
	}

	var ids []string
	s.registry.Mu.RLock()
	for id, r := range s.registry.Roles {
		// A planner must not assign work to itself or to the arbiter — that
		// would create planning loops.
		if id == "task_planner" || id == "decision_arbiter" || id == "sop_generator" {
			continue
		}
		// orchestrator_default is injected by config.loader for EVERY config
		// (loader.go:364), so "it is in the registry" says nothing about the
		// deployment. It has no MCP bindings and allow_dynamic_mcps=true,
		// which is precisely the wrong executor for a task whose value comes
		// from AppWorld MCP calls. A planner that picks it has misread the
		// catalog; letting that through produces a step that silently cannot
		// do the work. Observed 2026-10-18 (gemini-3-flash on the 2+2 probe
		// emitted role_id=orchestrator_default + allowed_tools=[execute_code]).
		if id == "orchestrator_default" {
			continue
		}
		if r == nil {
			continue
		}
		allowed[id] = true
		ids = append(ids, id)
	}
	s.registry.Mu.RUnlock()

	if len(ids) == 0 {
		return "", allowed
	}
	sort.Strings(ids)

	var sb strings.Builder
	sb.WriteString("\n### Allowed role ids\n")
	for _, id := range ids {
		s.registry.Mu.RLock()
		bc := ""
		if r := s.registry.Roles[id]; r != nil {
			bc = strings.TrimSpace(r.BaseCapability)
		}
		s.registry.Mu.RUnlock()
		if bc != "" {
			sb.WriteString(fmt.Sprintf("- `%s` (%s)\n", id, firstLine(bc, 120)))
		} else {
			sb.WriteString(fmt.Sprintf("- `%s`\n", id))
		}
	}
	return sb.String(), allowed
}

// firstLine collapses whitespace and truncates, for prompt readability.
func firstLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// plannerRoleInstruction returns the instruction of the task_planner role, or
// "" when the role is absent. Absence is not an error at this layer — callers
// treat it as "planning unavailable" and continue with the original inputs.
func (s *DirectedEngine) plannerRoleInstruction() string {
	if s.registry == nil {
		return ""
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	r, ok := s.registry.Roles["task_planner"]
	if !ok || r == nil {
		return ""
	}
	return r.Instruction
}

// plannerFirstEnabled reports whether tier 1(b) planning is switched on.
func (s *DirectedEngine) plannerFirstEnabled() bool {
	if s.registry == nil {
		return false
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	return s.registry.System.EnablePlannerFirst
}

// plannerParseOptions builds the validation options for a given target role.
func (s *DirectedEngine) plannerParseOptions(targetRole string, catalog map[string]map[string]bool) schemas.PlanParseOptions {
	_, roles := s.plannerAllowedRoles()

	single := ""
	if s.registry != nil {
		s.registry.Mu.RLock()
		if r, ok := s.registry.Roles[targetRole]; ok && r != nil && len(r.BoundMCPBindings) == 1 {
			single = r.BoundMCPBindings[0].MCPID
		}
		s.registry.Mu.RUnlock()
	}

	maxSteps := 0
	if s.registry != nil {
		s.registry.Mu.RLock()
		maxSteps = s.registry.System.PlannerMaxSteps
		s.registry.Mu.RUnlock()
	}

	return schemas.PlanParseOptions{
		AllowedRoles: roles,
		ToolCatalog:  catalog,
		SingleMCP:    single,
		MaxSteps:     maxSteps,
	}
}

// ─── Tier 1(b): pre-submission planning ───────────────────────────────────
//
// RequestPlanning runs the task_planner role once, before any step exists,
// and returns a validated plan. This is the entry point behind
// system.enable_planner_first (see S6 in docs 52-planner-wiring-design.md).
//
// Contract:
//   - Never returns an error. Planning is best-effort; the caller keeps its
//     original single step on any failure.
//   - Returns (nil, reason) when planning is unavailable (flag off, role
//     missing, empty task). Callers log the reason but proceed unchanged.
//   - Returns (steps, "") only for a plan that already passed
//     schemas.ParsePlanDAG against the LIVE tool catalog.
//
// Why best-effort rather than fatal: a planner call is an extra LLM round
// trip on the critical path. Turning "the planner was slow/garbled" into a
// submission failure would make the system strictly less reliable than not
// planning at all.
func (s *DirectedEngine) RequestPlanning(taskID, roleID, task string, timeoutSecs int) ([]schemas.StepInput, string) {
	if s.registry == nil || s.spawner == nil {
		return nil, "engine not initialised"
	}
	if !s.plannerFirstEnabled() {
		return nil, "enable_planner_first is off"
	}
	instruction := s.plannerRoleInstruction()
	if instruction == "" {
		// Registered role missing. This is the state of every AppWorld
		// config as of 2026-10-18 — see BUILDINFO "BEFORE RUNNING A BATCH".
		return nil, "task_planner role not present in config"
	}
	if strings.TrimSpace(task) == "" {
		return nil, "empty task"
	}

	roleList, allowedRoles := s.plannerAllowedRoles()
	toolCatalog, catalog := s.plannerToolCatalog(roleID)

	var sb strings.Builder
	sb.WriteString("You are a Task Planner for a supervised execution engine.\n")
	sb.WriteString("A weak main agent has delegated DAG design to you. Break the user request\n")
	sb.WriteString("below into an executable dependency graph.\n\n")
	sb.WriteString("## User request\n")
	sb.WriteString(task)
	sb.WriteString("\n")
	sb.WriteString(roleList)
	sb.WriteString(toolCatalog)
	sb.WriteString(planJSONContract)
	sb.WriteString("\n\nAdditional rules for this call:\n")
	sb.WriteString("- Prefer the FEWEST steps that still express real parallelism.\n")
	sb.WriteString("- Every step's \"task\" must be self-contained: assume the executing\n")
	sb.WriteString("  agent sees only that text plus its own tool list.\n")
	sb.WriteString("- Do NOT emit a verification/audit step. Verification is handled by\n")
	sb.WriteString("  the engine, not by the plan.\n")

	if timeoutSecs <= 0 {
		timeoutSecs = 90
		if s.registry.System.PlannerTimeoutSecs > 0 {
			timeoutSecs = s.registry.System.PlannerTimeoutSecs
		}
	}

	ctx, cancel := context.WithTimeout(s.lifecycleCtx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	stepID := "plan_" + taskID
	if taskID == "" {
		stepID = "plan_probe"
	}
	res, err := s.spawner.Spawn(ctx, &SpawnRequest{
		TaskID:      taskID,
		StepID:      stepID,
		RoleID:      "task_planner",
		Task:        sb.String(),
		RoutingMode: schemas.RoutingModeLegacy,
	})
	if err != nil || res == nil {
		reason := "planner spawn returned nothing"
		if err != nil {
			reason = "planner spawn failed: " + err.Error()
		}
		return nil, reason
	}

	raw := plannerResultText(res)
	if strings.TrimSpace(raw) == "" {
		return nil, "planner returned empty output"
	}

	steps, perr := schemas.ParsePlanDAG(raw, schemas.PlanParseOptions{
		AllowedRoles: allowedRoles,
		ToolCatalog:  catalog,
		SingleMCP:    s.plannerSingleMCP(roleID),
		MaxSteps:     s.plannerMaxSteps(),
	})
	if perr != nil {
		s.logger.Log("planner_first_rejected", taskID, "", map[string]any{
			"reason": perr.Error(),
			"raw":    truncateForLog(raw, 2000),
		})
		return nil, perr.Error()
	}
	if len(steps) == 0 {
		return nil, "planner produced no steps"
	}

	s.logger.Log("planner_first_ok", taskID, "", map[string]any{
		"steps":      len(steps),
		"target_role": roleID,
	})
	return steps, ""
}

// plannerSingleMCP returns the MCP id assumed for steps that omit mcp_id.
// Only unambiguous when the role binds exactly one MCP; otherwise "" which
// forces the plan to name mcp_id explicitly.
func (s *DirectedEngine) plannerSingleMCP(roleID string) string {
	if s.registry == nil {
		return ""
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	if r, ok := s.registry.Roles[roleID]; ok && r != nil && len(r.BoundMCPBindings) == 1 {
		return r.BoundMCPBindings[0].MCPID
	}
	return ""
}

// plannerMaxSteps reads the configured DAG budget (0 => parser default 12).
func (s *DirectedEngine) plannerMaxSteps() int {
	if s.registry == nil {
		return 0
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	return s.registry.System.PlannerMaxSteps
}

// planTerminalStepIDs returns the ids of steps nothing else depends on.
// Used to decide where a caller's Deliver contract is reattached after a
// planner-produced DAG replaces the original single step.
func planTerminalStepIDs(steps []schemas.StepInput) map[string]bool {
	hasDependent := make(map[string]bool, len(steps))
	for _, st := range steps {
		for _, dep := range st.DependsOn {
			hasDependent[dep] = true
		}
	}
	out := make(map[string]bool, len(steps))
	for _, st := range steps {
		if !hasDependent[st.ID] {
			out[st.ID] = true
		}
	}
	// A fully cyclic input would produce an empty terminal set; the parser
	// already rejects cycles, so this is only defensive.
	if len(out) == 0 {
		for _, st := range steps {
			out[st.ID] = true
		}
	}
	return out
}

// inheritCallerSettings copies per-run settings from the original submission
// onto the terminal steps of a planner-produced DAG. These settings are not
// part of the plan — they control how the step is executed and verified, not
// what the step does. Intermediate steps do NOT inherit them: e.g. the caller's
// Deliver contract belongs on the final step, not on every fan-out branch.
//
// T6 fix (2026-10-18): the original inline loop only inherited AdditionalSkills
// and Deliver, silently dropping ContextRefs, VerifierModel, MaxAutoRefine,
// EnableDebate, and MaxDebateRounds. A planner-produced terminal step that lost
// its ContextRefs could not receive upstream results; one that lost
// VerifierModel fell back to the default verifier, changing audit semantics.
func inheritCallerSettings(planned []schemas.StepInput, original schemas.StepInput) {
	inheritedIDs := planTerminalStepIDs(planned)
	for i := range planned {
		if !inheritedIDs[planned[i].ID] {
			continue
		}
		planned[i].AdditionalSkills = original.AdditionalSkills
		planned[i].Deliver = original.Deliver
		planned[i].ContextRefs = original.ContextRefs
		planned[i].VerifierModel = original.VerifierModel
		planned[i].MaxAutoRefine = original.MaxAutoRefine
		planned[i].EnableDebate = original.EnableDebate
		planned[i].MaxDebateRounds = original.MaxDebateRounds
	}
}
