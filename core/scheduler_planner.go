package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
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

// plannerParseAttempts bounds how many times RequestPlanning asks the model for
// a plan. One retry, not more: the observed failure is a formatting lapse, not
// a capability gap, and every extra attempt is a full LLM round trip on the
// critical path before the first step even starts.
const plannerParseAttempts = 2

// plannerRetryNudge is appended to the prompt on the retry. It shows the model
// its own opening bytes so "you did not produce JSON" is checkable rather than
// a claim it has to trust.
func plannerRetryNudge(prev string) string {
	prev = strings.TrimSpace(prev)
	if prev == "" {
		prev = "<empty>"
	}
	if len(prev) > 400 {
		prev = prev[:400] + "…"
	}
	return "## Retry — your previous answer was rejected\n\n" +
		"It could not be parsed as a JSON object. It began with:\n\n" +
		prev + "\n\n" +
		"Discard that answer completely. Reply again with ONLY the JSON object " +
		"defined by the Output Contract above: no <thought> block, no " +
		"explanation, no numbered outline, no markdown fence. Your first " +
		"character must be { and you must stop after the closing }."
}

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
   authenticate first, the login tool must be listed too.
8. No reasoning preamble. Do NOT open with a <thought> block, an analysis, or a
   numbered outline. The first character of your answer must be "{" and the
   last must be "}".`

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
func (s *DirectedEngine) plannerToolCatalog(roleID string, submitterIsAdmin bool) (string, map[string]map[string]bool) {
	catalog := map[string]map[string]bool{}
	if s.registry == nil {
		return "", catalog
	}

	// Phase 4 integration (2026-10-20): delegate scope enforcement to the
	// shared enforceScope function from discovery.go. Same visibility logic
	// as the discover MCP tool — single source of truth for scoping.
	// See docs/completed/2026-10-21/BUILTIN_PLAN_ROLES_OVERLAP_ANALYSIS.md §5 Phase 4.
	mcpIDs, scopeErr := enforceScope(s.registry, DiscoveryScope{Mode: "role", RoleID: roleID, AllowPool: false})
	if scopeErr != nil {
		s.logger.Log("EventPlannerEmptyCatalog", "", roleID, map[string]any{
			"role_id": roleID,
			"reason":  "scope_error: " + scopeErr.Error(),
		})
	}
	if len(mcpIDs) == 0 {
		// D2 fix (2026-10-20): Privilege escalation — do NOT fall back to
		// the entire MCP pool. A role with no BoundMCPBindings should see an
		// empty catalog. If the role has AllowDynamicMCPs=true, it will
		// receive MCPs at execution time via AdditionalMCPs; the spawner
		// will discover and route them then. The planner can still assign
		// work to this role (omit allowed_tools); it just can't pre-specify
		// tool names it hasn't discovered yet.
		s.logger.Log("EventPlannerEmptyCatalog", "", roleID, map[string]any{
			"role_id": roleID,
			"reason":  "no_bound_mcp_bindings",
		})
	}

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
		// Stage 5 (admin-hiding, 2026-10-24): drop admin_only MCPs from the
		// catalog for non-admin submitters so admin tool names never reach
		// the plan (and thus the public-visible task status).
		if !submitterIsAdmin && mcpDef.EffectiveMountPolicy() == config.MountPolicyAdminOnly {
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
//
// Phase 4 integration (2026-10-20): delegates role discovery to DiscoverImpl,
// sharing the discovery logic with the discover MCP tool. MCP ID enrichment
// still reads from the registry (DiscoveryRoleSummary doesn't include MCP IDs).
// See docs/completed/2026-10-21/BUILTIN_PLAN_ROLES_OVERLAP_ANALYSIS.md §5 Phase 4.
func (s *DirectedEngine) plannerAllowedRoles() (string, map[string]bool) {
	allowed := map[string]bool{}
	if s.registry == nil {
		return "", allowed
	}

	result, err := DiscoverImpl(s.registry, DiscoveryScope{Mode: "pool", AllowPool: true}, "roles", "", 1, 1000)
	if err != nil || result == nil {
		return "", allowed
	}

	summaryMap := make(map[string]DiscoveryRoleSummary, len(result.Roles))
	var ids []string
	for _, summary := range result.Roles {
		// A planner must not assign work to itself or to the arbiter — that
		// would create planning loops.
		if summary.ID == "task_planner" || summary.ID == "decision_arbiter" || summary.ID == "sop_generator" {
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
		if summary.ID == "orchestrator_default" {
			continue
		}
		allowed[summary.ID] = true
		ids = append(ids, summary.ID)
		summaryMap[summary.ID] = summary
	}

	if len(ids) == 0 {
		return "", allowed
	}
	sort.Strings(ids)

	// Batch MCP ID lookup with a single lock acquisition.
	s.registry.Mu.RLock()
	mcpIDsByRole := make(map[string][]string, len(ids))
	for _, id := range ids {
		if r := s.registry.Roles[id]; r != nil {
			for _, b := range r.BoundMCPBindings {
				mcpIDsByRole[id] = append(mcpIDsByRole[id], b.MCPID)
			}
		}
	}
	s.registry.Mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("\n### Allowed role ids\n")
	for _, id := range ids {
		summary := summaryMap[id]
		bc := strings.TrimSpace(summary.BaseCapability)
		if bc != "" {
			sb.WriteString(fmt.Sprintf("- `%s` (%s)", id, firstLine(bc, 120)))
		} else {
			sb.WriteString(fmt.Sprintf("- `%s`", id))
		}
		// Enrich with tool count (from DiscoverImpl) and bound MCP IDs
		// (from registry). Helps the planner make informed role assignments
		// without a separate discover call per role.
		mcpIDs := mcpIDsByRole[id]
		if len(mcpIDs) > 0 {
			sb.WriteString(fmt.Sprintf(" [tools: %d, mcps: %s]", summary.ToolCount, strings.Join(mcpIDs, ",")))
		}
		sb.WriteString("\n")
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

// rejectSimpleTasksEnabled reports whether the validateTaskComplexity guard
// (reject single-step local tasks with no skills/MCPs) is opt-in enabled.
// Default false: the Smart Routing Fast-Path already handles such tasks with
// near-zero overhead, so the guard is off by default to let them through.
// Set RejectSimpleTasks=true in system config to restore the legacy
// hard-reject behaviour for local (llama.cpp/vLLM) deployments.
func (s *DirectedEngine) rejectSimpleTasksEnabled() bool {
	if s.registry == nil {
		return false
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	return s.registry.System.RejectSimpleTasks
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
func (s *DirectedEngine) RequestPlanning(taskID, roleID, task string, timeoutSecs int, submitterIsAdmin bool) ([]schemas.StepInput, string) {
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

	// ── Catalog freshness (ADDED 2026-10-19) ──────────────────────────────
	// The planner runs before any step exists, so nothing has discovered the
	// target role's MCPs yet and plannerToolCatalog below would render an empty
	// catalog — which ParsePlanDAG rejects as "tool catalog unavailable". The
	// planner is the one component expected to see every capability, so its
	// catalog must not depend on a step having executed first.
	//
	// Discovery is cached per MCPDef and idempotent, so the first step of the
	// plan does not pay for it twice.
	discSecs := 90
	if s.registry.System.PlannerTimeoutSecs > 0 {
		discSecs = s.registry.System.PlannerTimeoutSecs
	}
	discCtx, discCancel := context.WithTimeout(s.lifecycleCtx, time.Duration(discSecs)*time.Second)
	discovered := s.spawner.ensureMCPDiscoveryForRole(discCtx, roleID, taskID)
	discCancel()
	if discovered > 0 {
		s.logger.Log("planner_catalog_discovered", taskID, "", map[string]any{
			"mcps": discovered,
		})
	}

	roleList, allowedRoles := s.plannerAllowedRoles()
	toolCatalog, catalog := s.plannerToolCatalog(roleID, submitterIsAdmin)

	// Count total tools across all MCPs in the catalog so the planner's
	// output budget can scale with tool-set size. A fixed 8192-token cap
	// truncates plans once the catalog exceeds ~100 tools (measured at
	// 453 tools in production, 2026-10-20). See plannerMaxOutputTokens.
	toolCount := 0
	for _, tools := range catalog {
		toolCount += len(tools)
	}

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
	// 🔴 Do NOT reintroduce a "prefer the fewest steps" rule here. It used to
	// read exactly that, and it fights the role instruction: the planner is
	// told (in config) that even a plain read belongs in its own step, then
	// told here to minimise step count, and the later instruction wins. That
	// produced the observed degeneration — "I'll use a single appworld_agent
	// step as it's a cohesive task" (task_96ac531e, 2026-10-19).
	//
	// The rule below is what both layers now say: a step boundary is a CONTEXT
	// decision. Split when a step's raw output is large and only its conclusion
	// is needed downstream — that is the entire reason to run work in a
	// subagent instead of inline. Do not split for tidiness.
	//
	// Measured 2026-10-19 (4fab96f_3): the "always split read from write" rule
	// produced a 3-step plan whose steps re-queried the same data, scoring 1/8,
	// while a single step scored 8/8 on the same task. A one-step plan is
	// therefore legitimate, and planner_first_single_step counts it — the thing
	// worth detecting is a planner that says one step EVERY time, not one that
	// says it when it is right.
	sb.WriteString("- Step count follows context shape, not tidiness. Split only when a\n")
	sb.WriteString("  step's raw output would otherwise flood the downstream context, or when\n")
	sb.WriteString("  two units need disjoint toolsets. A single step is a valid answer when\n")
	sb.WriteString("  the work is genuinely one or two calls.\n")
	sb.WriteString("- Every step's \"task\" must be self-contained: assume the executing\n")
	sb.WriteString("  agent sees only that text plus its own tool list.\n")
	sb.WriteString("- Do NOT emit a verification/audit step. Verification is handled by\n")
	sb.WriteString("  the engine, not by the plan.\n")
	// 🔴 2026-10-21 (docs/54 §5.25): two rules were added here and then REMOVED.
	// They read "describe the GOAL, not a call sequence" and "do NOT name a page
	// size". They did fix the filter-scope loss — one trial went 3/8 -> 7/8 —
	// but they also changed HOW MANY STEPS the planner emits:
	//
	//   08:13-08:56 (no rules): steps = 1, 1, 1, 1, 1
	//   08:59-09:20 (rules):    steps = 2, 1, 2, 2, 2
	//
	// A planner that splits a two-call task stops taking the smart-routing
	// fast-path, so the engine starts paying orchestration overhead for a task
	// that does not need it. Step count decides whether orchestration happens
	// at all; it is far more load-bearing than task wording.
	//
	// The same loss is covered without touching step count by the fix in
	// scheduler_submit.go: when a plan is a single step, the caller's task is
	// used verbatim, so a filtered request is never rewritten into a pipeline.
	// Do not reintroduce wording rules here without re-measuring step count.

	if timeoutSecs <= 0 {
		timeoutSecs = 90
		if s.registry.System.PlannerTimeoutSecs > 0 {
			timeoutSecs = s.registry.System.PlannerTimeoutSecs
		}
	}

	ctx, cancel := context.WithTimeout(s.lifecycleCtx, time.Duration(timeoutSecs*plannerParseAttempts)*time.Second)
	defer cancel()

	stepID := "plan_" + taskID
	if taskID == "" {
		stepID = "plan_probe"
	}

	basePrompt := sb.String()
	var (
		raw        string
		steps      []schemas.StepInput
		lastReason string
	)
	for attempt := 1; attempt <= plannerParseAttempts; attempt++ {
		prompt := basePrompt
		if attempt > 1 {
			// Do not re-send the identical prompt: a model that ignored the
			// contract once will ignore it again. Name the violation and quote
			// its own opening bytes back at it.
			prompt = basePrompt + "\n\n" + plannerRetryNudge(raw)
			s.logger.Log("planner_first_retry", taskID, "", map[string]any{
				"attempt": attempt,
				"reason":  lastReason,
			})
		}

		res, err := s.spawner.Spawn(ctx, &SpawnRequest{
			TaskID:      taskID,
			StepID:      stepID,
			RoleID:      "task_planner",
			Task:        prompt,
			RoutingMode: schemas.RoutingModeLegacy,
			// A plan overflows a prose-sized answer cap; see
			// plannerMaxOutputTokens for the measurement.
			MaxOutputTokens: s.plannerMaxOutputTokens(toolCount),
		})
		if err != nil || res == nil {
			reason := "planner spawn returned nothing"
			if err != nil {
				reason = "planner spawn failed: " + err.Error()
			}
			return nil, reason
		}

		raw = unwrapPlanEnvelope(plannerResultText(res))
		if strings.TrimSpace(raw) == "" {
			lastReason = "planner returned empty output"
			steps = nil
			continue
		}

		var perr error
		steps, perr = schemas.ParsePlanDAG(raw, schemas.PlanParseOptions{
			AllowedRoles: allowedRoles,
			ToolCatalog:  catalog,
			SingleMCP:    s.plannerSingleMCP(roleID),
			MaxSteps:     s.plannerMaxSteps(),
		})
		if perr == nil && len(steps) > 0 {
			// Sufficiency, not just parseability. A plan that parses and covers
			// only the preparation is the most expensive failure mode there is:
			// every step succeeds, the agent behaves, and the task is unmet.
			if gap := planCoverageGap(task, steps); gap != "" {
				lastReason = gap
				s.logger.Log("planner_coverage_gap", taskID, "", map[string]any{
					"reason":  gap,
					"steps":   len(steps),
					"attempt": attempt,
				})
				steps = nil
				continue
			}
			lastReason = ""
			break
		}
		if perr != nil {
			lastReason = perr.Error()
		} else {
			lastReason = "planner produced no steps"
		}
		steps = nil
	}

	if len(steps) == 0 {
		reason := lastReason
		if reason == "" {
			reason = "planner produced no usable plan"
		}
		s.logger.Log("planner_first_rejected", taskID, "", map[string]any{
			"reason":   reason,
			"raw":      truncateForLog(raw, 2000),
			"attempts": plannerParseAttempts,
		})
		return nil, reason
	}

	s.logger.Log("planner_first_ok", taskID, "", map[string]any{
		"steps":       len(steps),
		"target_role": roleID,
	})

	// Degeneration signal (ADDED 2026-10-19): a planner that always answers
	// "one step" has quietly turned the DAG path into the smart-routing
	// fast-path. That verdict is legitimate for a genuinely atomic request, but
	// it has to be *counted*, otherwise "planning never happens" stays
	// invisible. Compare this event's count against planner_first_ok per batch.
	if len(steps) == 1 {
		s.logger.Log("planner_first_single_step", taskID, "", map[string]any{
			"role":   roleID,
			"detail": "planner judged one step sufficient; a DAG adds nothing here",
		})
	}
	return steps, ""
}

// unwrapPlanEnvelope peels the output wrappers a planner model puts around the
// plan it was asked for.
//
// Measured 2026-10-19 (gemini-3-flash, AppWorld 4fab96f_3): asked for a bare
// {"steps": [...]}, the planner answered with the engine's own subagent
// envelope —
//
//	{"status":"ok","confidence":1.0,"result":{"steps":[...]}}
//
// — and, because the spawner had already wrapped that once, plannerResultText
// handed back {"raw":"<the envelope as an escaped string>"}. ParsePlanDAG then
// rejected a perfectly good plan with "plan contains no steps". The plan inside
// was better than anything the fast-path produced: it had already selected
// phone__get_current_date_and_time and the phone contact tools for step 1.
//
// Only wrappers are removed. The moment an object has a "steps" key it is
// returned verbatim, so a plan whose own content happens to contain keys named
// "result" or "content" is never mistaken for a wrapper.
func unwrapPlanEnvelope(raw string) string {
	s := strings.TrimSpace(raw)
	for depth := 0; depth < 4; depth++ { // bounded: wrappers nest shallowly
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return s // not JSON yet (markdown fences etc.) — leave it to the parser
		}
		if _, ok := m["steps"]; ok {
			return s
		}
		var next any
		found := false
		for _, k := range []string{"raw", "result", "output", "content", "answer", "text"} {
			if v, ok := m[k]; ok {
				next, found = v, true
				break
			}
		}
		if !found {
			return s
		}
		switch v := next.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				return s
			}
			s = v
		case map[string]any:
			b, err := json.Marshal(v)
			if err != nil {
				return s
			}
			s = string(b)
		default:
			return s
		}
	}
	return s
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

// plannerMaxOutputTokens returns the response token cap for one planner call.
//
// 8192, not the engine's default 4096: a plan is a structured document — one
// object per step, each carrying a self-contained task description plus an
// explicit tool list. Measured 2026-10-19 (gemini-3-flash, AppWorld 4fab96f_3),
// a 6-step plan was cut off at exactly 4096 tokens in the middle of a tool
// name; jsonrepair then closed the fragment into the syntactically valid string
// "venmo__", and the plan was rejected for an unknown tool after two LLM round
// trips. Nothing about that failure was the model's fault.
//
// §5.6 fix (2026-10-20): measured at 453 tools (AppWorld full surface) on
// 4fab96f_3, a successful planner call used 8,063 completion tokens against the
// 8,192 cap — 98.4%, a 129-token margin. It did NOT overrun: ran with
// output_truncated absent and stop_reason != "length". So 8192 does not
// truncate at 453 tools, but it is "just barely enough" rather than
// comfortable, and whether a given plan fits depends on how long the model
// chooses to write (the same task's first attempt used only 1,655 and stopped
// early; the second used 8,063). A budget that is one shape-change away from
// failure is a latent bug even while every observation passes.
//
// Hence: hold 8192 up to 100 tools (that regime was measured working at 60
// tools), then scale so the margin grows with the tool surface instead of
// staying at 1.6%. ~32 tokens per extra tool is a heuristic for the
// allowed_tools listing overhead, not a measurement; revisit if
// output_truncated ever fires below the cap.
func (s *DirectedEngine) plannerMaxOutputTokens(numTools int) int {
	if s.registry == nil {
		return 8192
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	if s.registry.System.PlannerMaxOutputTokens > 0 {
		return s.registry.System.PlannerMaxOutputTokens
	}
	if numTools <= 100 {
		return 8192
	}
	scaled := 8192 + (numTools-100)*32
	if scaled > 32768 {
		return 32768
	}
	return scaled
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
