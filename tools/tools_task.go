package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Helper functions from tools.go
func parseSessionRoles(v any) []*config.Role {
	if v == nil {
		return nil
	}
	roles, ok := v.([]*config.Role)
	if !ok {
		return nil
	}
	return roles
}

func parseSessionSkills(v any) []*config.Skill {
	if v == nil {
		return nil
	}
	skills, ok := v.([]*config.Skill)
	if !ok {
		return nil
	}
	return skills
}

// shouldRouteToFulfill determines if submit_decision routes to FulfillStep
// (true) or SubmitDecisionWithPayload (false). T1: replan/create_role/
// rewrite_dag/goal_change_requested carry structured payload and must go
// through SubmitDecisionWithPayload even when output is non-empty.
func shouldRouteToFulfill(choice, output string) bool {
	return (choice == "fulfill" || output != "") && choice != "rewrite_dag" && choice != "replan" && choice != "create_role" && choice != "goal_change_requested"
}

// validateTierChoice returns an error if the given choice is not permitted
// for the given tier. T2: replan/create_role are open to public tier but
// public callers cannot inject payload (replan uses pre-computed planner
// plan, create_role uses GenerateRoleObjects).
func validateTierChoice(tier, choice, output string) error {
	if tier != TierPublic {
		return nil
	}
	// Single source of truth for the public-tier option set lives in core
	// (PublicTierAllowsChoice) so the engine neither OFFERS (addDecision) nor
	// ACCEPTS (here) an option the public tier cannot choose (AUTH-3).
	switch choice {
	case "replan":
		if output != "" {
			return fmt.Errorf("public tier replan must not provide output payload (injection risk: use pre-computed planner plan)")
		}
	case "create_role":
		if output != "" {
			return fmt.Errorf("public tier create_role must not provide role draft payload (injection risk: use GenerateRoleObjects)")
		}
	}
	if core.PublicTierAllowsChoice(choice) {
		return nil
	}
	return fmt.Errorf("choice %q requires admin tier (public tier allows skip|abort|retry|refine_and_retry|fulfill|resume_more_turns|replan|create_role)", choice)
}

func registerTaskList(s *server.MCPServer, app *App) {
	submitDesc := `Delegates complex or specialized tasks to a supervised execution engine.
Plans multi-step work and coordinates across specialized professional roles.

Key Philosophy:
- Professional Tasks to Professional Roles: Avoid cluttering the main system prompt with diverse knowledge.
- Context Isolation: Each task step runs with a specialized Role's prompt, keeping execution focused and context-efficient.

When to use:
- Multi-step tasks or complex planning.
- Coordinating specialized roles and tools (call orchestrator_discover to inspect available capabilities).
- Long-running execution or background processing.

When NOT to use:
- Single-step tasks that can be completed directly (e.g., one filesystem read, one search, one calculation).`
	app.register(s, mcp.NewTool("orchestrator_submit_task",
		mcp.WithDescription(submitDesc+`
Example Usage:
orchestrator_submit_task(steps:[{"id":"s1","role_id":"software_engineer","task":"Write a hello world in Go"}]`),
		mcp.WithAny("steps", mcp.Description("Array of step objects. Required fields per step: 'task' (string). Example: [{\"id\":\"s1\",\"role_id\":\"software_engineer\",\"task\":\"...\"}]")),
		mcp.WithString("group_id", mcp.Description("Dispatch a RoleGroup by ID.")),
		mcp.WithString("task", mcp.Description("Task description")),
		mcp.WithAny("context_refs", mcp.Description("Context references")),
		mcp.WithAny("additional_mcps", mcp.Description("Additional MCP IDs")),
		mcp.WithAny("session_roles", mcp.Description("Session-specific roles")),
		mcp.WithAny("session_skills", mcp.Description("Session-specific skills")),
		mcp.WithString("session_id", mcp.Description("Session ID for workspace binding. All tasks in the same session share an authorized workspace root.")),
		mcp.WithString("workspace_root", mcp.Description("Absolute path to workspace root. IGNORED for public tier (auto-bound to session sandbox). Only honored for local (no API key) and admin tier callers.")),
		mcp.WithNumber("timeout", mcp.Description("Per-task timeout in seconds. Overrides the global default. 0 = use system default.")),
		mcp.WithNumber("token_budget", mcp.Description("Per-task token budget cap. Overrides the global default. 0 = use system default.")),
		mcp.WithAny("charter", mcp.Description("Optional Task Charter: {\"intent\": \"why\", \"non_goals\": [\"what not to do\"], \"constraints\": [\"what must hold\"]}. See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P1.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, _ := req.Params.Arguments.(map[string]any)
		sessionRoles := parseSessionRoles(args["session_roles"])
		sessionSkills := parseSessionSkills(args["session_skills"])
		sessionID := strArg(args, "session_id")
		requestedRoot := strArg(args, "workspace_root")
		timeoutSecs := intArg(args, "timeout", 0)
		tokenBudget := int64Arg(args, "token_budget", 0)

		// Tier-Gated Workspace Scope (ADDED 2026-10-13):
		// Public tier callers cannot escalate via workspace_root.
		// See docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §3.3.
		var workspacePolicy *WorkspacePolicy
		if wp := app.Registry.GetWorkspacePolicy(CallerOwnerID(ctx)); wp != nil {
			workspacePolicy = &WorkspacePolicy{
				OwnerID:            CallerOwnerID(ctx),
				AllowedRoots:       wp.AllowedRoots,
				SessionSandboxBase: wp.SessionSandboxBase,
			}
		}
		workspaceRoot, err := ResolveWorkspaceRoot(ctx, workspacePolicy, requestedRoot)
		if err != nil {
			return errResult(err.Error())
		}
		if app.Logger != nil && requestedRoot != workspaceRoot {
			app.Logger.Log("workspace_root_resolved", "", "", map[string]any{
				"tier":      ctx.Value(AuthTierKey),
				"owner":     CallerOwnerID(ctx),
				"requested": requestedRoot,
				"effective": workspaceRoot,
			})
		}

		groupID := strArg(args, "group_id")
		if groupID != "" {
			taskStr := strArg(args, "task")
			ctxRefs := make(map[string]string)
			if raw, ok := args["context_refs"]; ok && raw != nil {
				if m, ok := raw.(map[string]any); ok {
					for k, v := range m {
						if sv, ok := v.(string); ok {
							ctxRefs[k] = sv
						}
					}
				}
			}
			dispatcher := core.NewGroupDispatcher(app.Scheduler, app.Registry)
			id, err := dispatcher.DispatchGroup(groupID, taskStr, ctxRefs, sessionRoles, sessionSkills, nil, "", EffectiveTier(ctx, app) == TierAdmin, CallerOwnerID(ctx))
			if err != nil {
				return errResult(err.Error())
			}
			return jsonOK(map[string]string{"task_id": id, "group_id": groupID})
		}

		stepsRaw := args["steps"]
		var inputs []schemas.StepInput
		var autoMatched map[string]any
		var policyNotifications []map[string]any

		// Parse additional_mcps from top-level args
		// Defect 4 fix: also accept "mcp_bindings" as an alias for "additional_mcps"
		var extraMCPs []string
		mcpRaw := args["additional_mcps"]
		if mcpRaw == nil {
			mcpRaw = args["mcp_bindings"]
		}
		if mcpRaw != nil {
			switch v := mcpRaw.(type) {
			case []string:
				extraMCPs = v
			case []any:
				for _, x := range v {
					if s, ok := x.(string); ok {
						extraMCPs = append(extraMCPs, s)
					}
				}
			case string:
				extraMCPs = []string{v}
			}
		}

		// SECURITY (2026-10-21): the blunt "non-admin ⇒ reject any additional_mcps"
		// check was REMOVED. The mount decision now lives in
		// core.ResolveFinalMCPs, which evaluates per-MCP: an MCP already bound to the
		// role is always allowed (fixing the G1/G11 side effect where public callers
		// were blocked from re-specifying their own role's MCPs); an unbound MCP is
		// gated by its MountPolicy (open / approval / admin_only). Delegating keeps a
		// single source of truth instead of two divergent gates.

		// Intent-Driven Implicit Discover & Submit
		// When steps is absent but task is provided, the caller gives a
		// high-level intent. We synthesize a single StepInput and
		// implicitly bind matching MCPs/roles without requiring the caller
		// to know the internal capability catalog.
		taskStr := strArg(args, "task")

		// Parse top-level context_refs if present (shared by both paths).
		// audit fix: previously this was inside the implicit single-step
		// branch only — on the explicit steps path, top-level context_refs
		// was silently dropped (additional_mcps had a merge, context_refs
		// did not).
		ctxRefs := make(map[string]string)
		if raw, ok := args["context_refs"]; ok && raw != nil {
			if m, ok := raw.(map[string]any); ok {
				for k, v := range m {
					if sv, ok := v.(string); ok {
						ctxRefs[k] = sv
					}
				}
			}
		}

		if stepsRaw == nil && taskStr != "" {
			si := schemas.StepInput{
				ID:          "s1",
				Task:        taskStr,
				ContextRefs: ctxRefs,
			}

			// Merge any explicitly provided additional_mcps
			if len(extraMCPs) > 0 {
				si.AdditionalMCPs = extraMCPs
			}

			// Smart Intent Routing REMOVED (2026-10-21, audit R2/D4). The old
			// substring table auto-mounted vendor MCPs (ghost-driver, docx-mcp,
			// ...) on words like "generate"/"image", running AFTER the tier gate
			// and hardcoding supplier names in core. Callers must now name
			// role_id / additional_mcps explicitly, or let the planner route.

			inputs = []schemas.StepInput{si}
		} else {
			// Standard path: steps provided explicitly
			if stepsRaw == nil {
				return errResult("steps is required. Example: orchestrator_submit_task(steps:[{\"id\":\"s1\",\"role_id\":\"software_engineer\",\"task\":\"Your task description here\"}])")
			}

			var stepsData []byte
			switch v := stepsRaw.(type) {
			case string:
				stepsData = []byte(v)
			default:
				stepsData, _ = json.Marshal(stepsRaw)
			}

			if err := json.Unmarshal(stepsData, &inputs); err != nil {
				return errResult(fmt.Sprintf("invalid steps format: %v. Expected JSON array of step objects, e.g. [{\"id\":\"s1\",\"task\":\"...\"}]", err))
			}
			if len(inputs) == 0 {
				return errResult("steps array cannot be empty. Example: steps:[{\"id\":\"s1\",\"role_id\":\"software_engineer\",\"task\":\"...\"}]")
			}

			// Merge top-level additional_mcps into each step
			if len(extraMCPs) > 0 {
				for i := range inputs {
					if len(inputs[i].AdditionalMCPs) == 0 {
						inputs[i].AdditionalMCPs = extraMCPs
					}
				}
			}

			// Merge top-level context_refs into each step (mirrors
			// additional_mcps merge above). Per-step context_refs take
			// precedence — top-level refs only apply to steps that don't
			// define their own.
			if len(ctxRefs) > 0 {
				for i := range inputs {
					if len(inputs[i].ContextRefs) == 0 {
						inputs[i].ContextRefs = ctxRefs
					}
				}
			}
		}

		// P0 Validation: ensure at least one step has a task description
		hasValidStep := false
		for _, inp := range inputs {
			if strings.TrimSpace(inp.Task) != "" {
				hasValidStep = true
				break
			}
		}
		if !hasValidStep {
			return errResult("at least one step must contain a non-empty 'task' field. Example: steps:[{\"id\":\"s1\",\"role_id\":\"software_engineer\",\"task\":\"Write tests\"}]")
		}

		// SECURITY FIX (2026-10-21, issue R1): record the submitter's tier so the
		// execution-path mount gate (core.ResolveFinalMCPs, via
		// TaskGraph.SubmitterIsAdmin) can enforce it — planner/replan/surgery
		// steps inherit it instead of being treated as admin. StepInput's field
		// is json:"-", so a caller cannot forge admin.
		submitterIsAdmin := EffectiveTier(ctx, app) == TierAdmin
		for i := range inputs {
			inputs[i].SubmitterIsAdmin = submitterIsAdmin
		}

		// Issue-3 (2026-10-23): capture the caller's ORIGINAL step count before
		// the Adaptive Verifier Policy below may append auditor steps. The
		// response reports both counts (submitted vs final) so an agent does not
		// mistake an intentional augmentation for a failed submit and resubmit.
		submittedStepCount := len(inputs)

		// ── Adaptive Verifier Policy (ADDED 2026-09-07) ─────────────────────
		// Auto-append an "auditor" step after each Critical-tier step, so the
		// existing auditor role verifies the output through the standard DAG
		// path. No VerifierModel hack, no ExitCriteria coupling — reuses the
		// normal step→result_ref pipeline.
		//
		// Notifications are surfaced in the SubmitTask response so Main Agent
		// knows exactly which steps got which risk tier.
		var augmentedInputs []schemas.StepInput
		for i := range inputs {
			orig := inputs[i]
			tier, rating := schemas.DetermineRiskTier(orig.Task, orig.AdditionalMCPs)
			orig.RiskRating = rating
			notif := map[string]any{
				"step_id":      orig.ID,
				"role_id":      orig.RoleID,
				"risk_tier":    string(tier),
				"risk_rating":  rating,
				"task_preview": orig.Task[:min(100, len(orig.Task))],
			}
			switch tier {
			case schemas.RiskTierCritical:
				auditID := orig.ID + "_audit"
				notif["appended_audit_step"] = auditID
				augmentedInputs = append(augmentedInputs, orig)

				// Determine if this step is a terminal asset step or an intermediate setup step
				taskLower := strings.ToLower(orig.Task)
				isTerminalStep := strings.Contains(taskLower, "download") ||
					strings.Contains(taskLower, "save") ||
					strings.Contains(taskLower, "export") ||
					strings.Contains(taskLower, "asset")
				isBrowserTask := strings.Contains(taskLower, "browser") ||
					strings.Contains(taskLower, "navigate") ||
					strings.Contains(taskLower, "url") ||
					strings.Contains(taskLower, "page")

				var auditTask string
				if isTerminalStep {
					auditTask = fmt.Sprintf(
						"Review the output of step %q.\n\nUPSTREAM STEP TASK:\n%s\n\nYou are the terminal exit-gate auditor. This step is expected to produce real file assets. Evaluate whether the output contains direct asset handles or valid absolute file paths produced by actual tool executions. Reject placeholder URLs and unexecuted steps. Respond ONLY with 'PASS' or 'FAIL: <category>: <reason>'. Categories: TIMEOUT, PERMISSION, CONFLICT, LOGIC, SCHEMA.",
						orig.ID, orig.Task,
					)
				} else if isBrowserTask {
					auditTask = fmt.Sprintf(
						"Review the output of step %q.\n\nUPSTREAM STEP TASK:\n%s\n\nYou are the intermediate checkpoint auditor. This is a setup/navigation step. Evaluate whether the browser connection or navigation succeeded (e.g. connection status connected, correct title/URL). Do NOT fail this step merely because no final image file was downloaded yet. Respond ONLY with 'PASS' or 'FAIL: <category>: <reason>'. Categories: TIMEOUT, PERMISSION, CONFLICT, LOGIC, SCHEMA.",
						orig.ID, orig.Task,
					)
				} else {
					auditTask = fmt.Sprintf(
						"Review the output of step %q.\n\nUPSTREAM STEP TASK:\n%s\n\nYou are the intermediate checkpoint auditor. This is a setup/preparatory step. Evaluate whether the step succeeded and its outputs are valid for downstream consumption (e.g. correct data structure, required fields present, no errors). Do NOT fail this step merely because it does not produce final deliverables. Respond ONLY with 'PASS' or 'FAIL: <category>: <reason>'. Categories: TIMEOUT, PERMISSION, CONFLICT, LOGIC, SCHEMA.",
						orig.ID, orig.Task,
					)
				}

				// C5 fix: filter write tools + deep copy to avoid mutation leak.
				auditMCPs, auditAllowlists := core.AuditorAllowedTools(orig.AdditionalMCPs, orig.AdditionalToolAllowlists)

				augmentedInputs = append(augmentedInputs, schemas.StepInput{
					ID:        auditID,
					RoleID:    "auditor",
					DependsOn: []string{orig.ID},
					Task:      auditTask,
					// U5 (2026-10-18): auditor inherits parent step's MCP
					// bindings so it can verify against the same data sources.
					// Universal: an auditor without fact-checking tools is
					// structurally biased toward PASS. See eval §3.2b(4).
					//
					// C5 fix (2026-10-18): filter write tools from the
					// inherited allowlists — the auditor should only have
					// read-only tools (it's verifying, not acting). Also
					// deep-copy to avoid mutation leak (shared map/slice
					// reference would let auditor mutations affect parent).
					AdditionalMCPs:           auditMCPs,
					AdditionalToolAllowlists: auditAllowlists,
				})
			case schemas.RiskTierModerate:
				notif["hint"] = "consider adding exit_criteria for structured verification"
				augmentedInputs = append(augmentedInputs, orig)
			default: // Light
				augmentedInputs = append(augmentedInputs, orig)
			}
			policyNotifications = append(policyNotifications, notif)
			app.Logger.Log("EventAdaptiveVerifierPolicy", "", orig.ID, notif)
		}
		inputs = augmentedInputs

		// Surface non-mandatory reference hints for single-step, unplanned tasks
		if len(inputs) == 1 && inputs[0].RoleID == "" && inputs[0].Task != "" {
			var routedCandidates []core.SOPRouteResult
			if app.IntentRouter != nil {
				if rc, err := app.IntentRouter.Route(ctx, app.Registry, inputs[0].Task, 3); err == nil {
					routedCandidates = rc
				}
			}
			if len(routedCandidates) == 0 {
				if candidates := core.MatchSOPCandidates(app.Registry, inputs[0].Task, 3); len(candidates) > 0 {
					for _, c := range candidates {
						routedCandidates = append(routedCandidates, core.SOPRouteResult{
							SOP:        c.SOP,
							Confidence: float64(c.Hits) / 10.0,
							MatchMode:  "keyword_fallback",
						})
					}
				}
			}
			if len(routedCandidates) > 0 {
				ids := make([]string, len(routedCandidates))
				for i, c := range routedCandidates {
					ids[i] = c.SOP.ID
				}
				app.Logger.Log(core.EventSOPCandidatesOffered, "", "", map[string]any{"sop_ids": ids, "match_mode": routedCandidates[0].MatchMode})
				var stdCandidates []core.SOPCandidate
				for _, rc := range routedCandidates {
					stdCandidates = append(stdCandidates, core.SOPCandidate{
						SOP:  rc.SOP,
						Hits: int(rc.Confidence * 10),
					})
				}
				inputs[0].Task += core.FormatSOPHints(stdCandidates)
			}
		}
		if app.ExpStore != nil {
			if jitCandidates := app.ExpStore.QueryJITCandidates(ctx, inputs[0].Task, 5, 0.7, 3); len(jitCandidates) > 0 {
				ids := make([]string, len(jitCandidates))
				for i, p := range jitCandidates {
					ids[i] = p.ID
				}
				app.Logger.Log(core.EventJITCompositionSuggested, "", "", map[string]any{"pattern_ids": ids})
				inputs[0].Task += store.FormatJITCompositionHints(jitCandidates)
			}
		}

		// Skill Discovery & Activation — run DiscoveryPipeline, activate top-3 skills.
		// Appends skill descriptions + manifest summaries to the task, following the
		// same pattern as SOP/JIT hints above.
		if app.Navigator != nil && len(inputs) > 0 && inputs[0].Task != "" {
			tags := app.Navigator.ClassifyIntent(ctx, inputs[0].Task)
			candidates := app.Navigator.Pipeline.Execute(ctx, inputs[0].Task, tags)
			skillHint := core.ActivateTopSkills(app.Registry, candidates, 3)
			if skillHint != "" {
				inputs[0].Task += skillHint
				app.Logger.Log("EventSkillsActivated", "", "", map[string]any{
					"candidate_count": len(candidates),
				})
			}
		}

		id, err := app.Scheduler.SubmitWithSessionIR(inputs, sessionRoles, sessionSkills, nil, "", sessionID, workspaceRoot, timeoutSecs, tokenBudget, CallerIdentity(ctx))
		if err != nil {
			return errResult(err.Error())
		}

		// Optional Task Charter (P1): parse and set on the graph if provided.
		if charterRaw, ok := args["charter"]; ok && charterRaw != nil {
			if charterMap, ok := charterRaw.(map[string]any); ok {
				charter := &schemas.TaskCharter{Version: 1}
				if v, ok := charterMap["intent"].(string); ok {
					charter.Intent = v
				}
				if v, ok := charterMap["non_goals"].([]any); ok {
					for _, ng := range v {
						if s, ok := ng.(string); ok {
							charter.NonGoals = append(charter.NonGoals, s)
						}
					}
				}
				if v, ok := charterMap["constraints"].([]any); ok {
					for _, c := range v {
						if s, ok := c.(string); ok {
							charter.Constraints = append(charter.Constraints, s)
						}
					}
				}
				app.Scheduler.SetCharter(id, charter)
			}
		}

		// Inline complexity hint (replaces former orchestrator_analyze_task)
		stepCount := len(inputs)
		hasMultipleRoles := false
		roleCount := 0
		for _, inp := range inputs {
			if inp.RoleID != "" {
				roleCount++
			}
		}
		if roleCount > 1 {
			hasMultipleRoles = true
		}
		hint := "single_step"
		if stepCount > 1 {
			hint = "multi_step_dag"
		} else if hasMultipleRoles {
			hint = "multi_role"
		}

		return jsonOK(map[string]any{
			"task_id":              id,
			"complexity_hint":      hint,
			"submitted_step_count": submittedStepCount,
			// step_count is the FINAL DAG size and may exceed submitted_step_count
			// when the Adaptive Verifier Policy appended auditor steps (see
			// adaptive_verifier[].appended_audit_step). added_step_count > 0 is a
			// successful, intentional augmentation — NOT a failed submit.
			"step_count":        stepCount,
			"added_step_count":  stepCount - submittedStepCount,
			"auto_matched":      autoMatched,
			"adaptive_verifier": policyNotifications,
		})
	})

	app.register(s, mcp.NewTool("orchestrator_wait_task",
		mcp.WithDescription("Wait for task completion."),
		mcp.WithString("task_id", mcp.Required()),
		mcp.WithNumber("timeout", mcp.Description("Seconds")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := strArg(req.Params.Arguments, "task_id")
		if DenyIfNotOwner(ctx, app, id) {
			return errResult("not found")
		}
		timeout := time.Duration(intArg(req.Params.Arguments, "timeout", 30)) * time.Second
		status, ok := app.Scheduler.WaitTask(ctx, id, timeout)
		if !ok {
			return errResult("not found")
		}
		return jsonOK(status)
	})

	app.register(s, mcp.NewTool("orchestrator_get_task_status",
		mcp.WithDescription("Poll task status. Returns summary by default (lightweight). Use view='full' for complete details including steps, global_workspace, and context_tree."),
		mcp.WithString("task_id", mcp.Required()),
		mcp.WithString("view", mcp.Description("summary (default) or full")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := strArg(req.Params.Arguments, "task_id")
		if DenyIfNotOwner(ctx, app, id) {
			return errResult("not found")
		}
		view := strArg(req.Params.Arguments, "view")
		if view == "" {
			view = "summary"
		}
		status, ok := app.Scheduler.GetStatus(id, view)
		if !ok {
			return errResult("not found")
		}
		return jsonOK(status)
	})

	app.register(s, mcp.NewTool("orchestrator_discover",
		mcp.WithDescription("Discover capabilities: roles, skills, MCPs, and their tools."),
		mcp.WithString("intent", mcp.Description("Item ID to explore")),
		mcp.WithString("query", mcp.Description("Keyword search")),
		mcp.WithString("subsystem", mcp.Description("Subsystem actions")),
		mcp.WithString("search_mode", mcp.Description("Search mode")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		intent := strArg(req.Params.Arguments, "intent")
		query := strArg(req.Params.Arguments, "query")
		sub := strArg(req.Params.Arguments, "subsystem")
		if intent != "" {
			if intent == "menu" {
				return mcp.NewToolResultText(app.Navigator.GenerateDynamicMenu(app.Registry)), nil
			}
			return mcp.NewToolResultText(app.Navigator.ExploreIntent(intent)), nil
		}
		if query != "" {
			return mcp.NewToolResultText(app.Navigator.Search(ctx, query)), nil
		}
		if sub != "" {
			doc, err := registry.discoverActions(sub, EffectiveTier(ctx, app))
			if err != nil {
				return errResult(err.Error())
			}
			return mcp.NewToolResultText(doc), nil
		}
		return mcp.NewToolResultText(app.Navigator.GetOverview()), nil
	})

	if app.Archive != nil {
		app.register(s, mcp.NewTool("orchestrator_context_search",
			mcp.WithDescription("Searches the Orchestrator's hot-data memory (persisted ContextTree nodes and GlobalWorkspace results). Returns either RRF-ranked top-k or a Kruskal-optimized non-redundant evidence forest. Single-task scope: task_scope is required and limits results to the specified task only."),
			mcp.WithString("query", mcp.Required()),
			mcp.WithNumber("k", mcp.Description("Number of results (default: 10)")),
			mcp.WithString("mode", mcp.Description("Search mode: rrf | forest (default: forest)")),
			mcp.WithNumber("redundancy", mcp.Description("Redundancy threshold for forest mode (default: 0.85)")),
			mcp.WithString("task_scope", mcp.Required(), mcp.Description("Task ID to limit search scope. Required — cross-task search is not allowed in open-core edition.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// orchestrator_context_search: active when ContextArchive is initialized.
			// Single-task scope enforced: task_scope is required and must be non-empty.
			// Cross-task search requires enterprise isolation layer (SessionID/OwnerRole).
			// See docs/completed/2026-09-13/CONTEXT_ARCHIVE_ACTIVATION_DESIGN.md.
			if app.Archive == nil {
				return errResult("Context archive not initialized")
			}
			query := strArg(req.Params.Arguments, "query")
			k := intArg(req.Params.Arguments, "k", 10)
			mode := strArg(req.Params.Arguments, "mode")
			if mode == "" {
				mode = "forest"
			}
			var redundancy float64 = 0.85
			if r, ok := req.Params.Arguments.(map[string]any)["redundancy"].(float64); ok {
				redundancy = r
			}
			taskScope := strArg(req.Params.Arguments, "task_scope")
			if taskScope == "" {
				return errResult("task_scope is required — cross-task search is not allowed in open-core edition")
			}
			if DenyIfNotOwner(ctx, app, taskScope) {
				return errResult("not found")
			}
			if app.Scheduler != nil && !app.Scheduler.TaskKnown(taskScope) {
				return errResult("task_scope does not match any known task — cross-task search is not allowed")
			}

			items := app.Archive.Search(query, app.EmbedClient, 24, taskScope, taskScope)
			if mode == "rrf" || len(items) == 0 {
				if len(items) > k {
					items = items[:k]
				}
				return jsonOK(items)
			}
			// Forest mode — delegate to core layer
			result, err := app.Archive.SearchForest(query, app.EmbedClient, k, redundancy, taskScope, taskScope)
			if err != nil {
				return errResult("SearchForest: " + err.Error())
			}
			return jsonOK(map[string]any{
				"nodes":  result.Nodes,
				"edges":  result.Edges,
				"forest": true,
			})
		})
	}

	app.register(s, mcp.NewTool("orchestrator_submit_decision",
		mcp.WithDescription("Respond to a blocked step decision. Choices: skip|abort|retry|refine_and_retry (public tier), retry_with_skill:ID|retry_with_context:TEXT|escalate_model:MODEL|create_skill|rewrite_dag|goal_change_requested (admin tier only). For rewrite_dag, pass surgery JSON in 'output' with deprecated_step_id and replacement_steps. For goal_change_requested, pass a TaskCharter JSON in 'output' with intent, non_goals, constraints. For delegation-required decisions, pass choice='fulfill' plus an 'output' JSON string containing the subagent/human result payload. Optionally attach 'human_approval' {enabled, evidence_text, evidence_ref} to log a human review trail."),
		mcp.WithString("task_id", mcp.Required()),
		mcp.WithString("decision_id", mcp.Required()),
		mcp.WithString("choice", mcp.Required()),
		mcp.WithString("output", mcp.Description("Optional. Only used with choice='fulfill' or delegation decisions: the subagent/human result payload as a JSON string.")),
		mcp.WithObject("human_approval", mcp.Description("Optional. Attach a structured human-review trail. Fields: enabled (bool), evidence_text (string), evidence_ref (string, e.g. @session:... or a URL). All fields optional; only effective when provided.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tid := strArg(req.Params.Arguments, "task_id")
		if DenyIfNotOwner(ctx, app, tid) {
			return errResult("not found")
		}
		did := strArg(req.Params.Arguments, "decision_id")
		choice := strArg(req.Params.Arguments, "choice")
		output := strArg(req.Params.Arguments, "output")

		args := map[string]any{}
		if req.Params.Arguments != nil {
			for k, v := range req.Params.Arguments.(map[string]any) {
				args[k] = v
			}
		}
		humanApproval, _ := args["human_approval"].(map[string]any)
		hasApproval := humanApproval != nil && len(humanApproval) > 0

		// Public tier choice allowlist.
		// T2: replan/create_role are open to public tier BUT public callers
		// cannot inject payload — replan uses pre-computed planner plan,
		// create_role uses GenerateRoleObjects.
		if err := validateTierChoice(EffectiveTier(ctx, app), choice, output); err != nil {
			return errResult(err.Error())
		}

		// Route: fulfill path if output provided, or if choice explicitly says "fulfill".
		// T1: replan/create_role/rewrite_dag carry structured payload and must
		// route through SubmitDecisionWithPayload, not FulfillStep.
		if shouldRouteToFulfill(choice, output) {
			if output == "" {
				return errResult("choice='fulfill' or output payload requires 'output' field with the result JSON")
			}
			if err := app.Scheduler.FulfillStep(tid, did, output); err != nil {
				return errResult(err.Error())
			}
		} else {
			if err := app.Scheduler.SubmitDecisionWithPayload(tid, did, choice, output); err != nil {
				return errResult(err.Error())
			}
		}

		// Attach human approval evidence to the task log if provided.
		// Non-blocking: logging failure does not fail the decision itself.
		if hasApproval && app.Logger != nil {
			evidenceText, _ := humanApproval["evidence_text"].(string)
			evidenceRef, _ := humanApproval["evidence_ref"].(string)
			enabled, _ := humanApproval["enabled"].(bool)
			app.Logger.Log(core.EventHumanApprovalAttached, tid, "", map[string]any{
				"decision_id":   did,
				"choice":        choice,
				"enabled":       enabled,
				"evidence_text": evidenceText,
				"evidence_ref":  evidenceRef,
			})
		}
		return jsonOK(map[string]any{
			"ok":             true,
			"human_approval": hasApproval,
		})
	})

	app.register(s, mcp.NewTool("orchestrator_get_logs",
		mcp.WithDescription("Retrieve execution logs for a specific task."),
		mcp.WithString("task_id", mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max events to return (default: 50, most recent first)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := strArg(req.Params.Arguments, "task_id")
		if DenyIfNotOwner(ctx, app, id) {
			return errResult("not found")
		}
		events, err := app.Logger.ReadTaskLogs(id)
		if err != nil {
			return errResult(err.Error())
		}
		limit := intArg(req.Params.Arguments, "limit", 50)
		if limit <= 0 {
			limit = 50
		}
		if len(events) > limit {
			events = events[len(events)-limit:]
		}
		return jsonOK(events)
	})
}
