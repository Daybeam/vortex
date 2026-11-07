package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

// handleSlashCommand checks if the user's message is a slash command (e.g.
// "/delegate ...", "/help", "/tools"). If so, it executes the command
// directly WITHOUT an LLM turn and returns true. If the message is not a
// slash command, returns false and the caller proceeds with the normal
// chat loop.
//
// Design rationale: when the human knows exactly which tool to use or wants
// to delegate directly, a slash command skips the chat agent's discovery
// turn entirely — saving latency, tokens, and avoiding the failure mode
// where the LLM picks the wrong tool. The /delegate command is the
// "invoke passthrough": it calls Harness.Delegate which submits to the
// orchestrator scheduler (full MCP access) without any chat-LLM inference.
//
// This lives in the handler layer (not ChatHarness) to preserve the
// harness's peelability — ChatHarness stays a pure LLM loop with no
// command parsing.
// humanApprovalDecisionDenied reports whether a pending decision is of class
// "human_approval_required" (schemas.DecisionHumanApprovalRequired) — a class
// the design requires to be approved by a web-page click, never via a chat
// channel. SECURITY (2026-10-22): /decision and /approve previously accepted
// any decision from any Telegram user, defeating that page-only intent.
func (h *ChatHandler) humanApprovalDecisionDenied(taskID, decisionID string) bool {
	if h.App == nil || h.App.Scheduler == nil {
		return false
	}
	status, ok := h.App.Scheduler.GetStatus(taskID, "full")
	if !ok {
		return false
	}
	decisions, _ := status["pending_decisions"].([]map[string]any)
	for _, d := range decisions {
		if did, _ := d["id"].(string); did == decisionID {
			// d["type"] holds a schemas.DecisionType (a named string type), so
			// a plain .(string) assertion fails — compare via fmt.Sprint.
			return fmt.Sprint(d["type"]) == string(schemas.DecisionHumanApprovalRequired)
		}
	}
	return false
}

// cmdMCP implements "/mcp" — list the MCP pool with each member's operator
// toggle state, and flip it. Admin-gated (adminCommands). The write path reuses
// the web UI's whole-definition replace (/api/config/update-mcp → CommitOps
// /mcps/{id}), so every unrelated field survives.
// See docs/plan/MCP_TOOL_TOGGLE_DESIGN.md.
func (h *ChatHandler) cmdMCP(arg string) (bool, string) {
	if h.App == nil || h.App.Registry == nil {
		return true, "Error: registry not available."
	}
	f := strings.Fields(arg)
	if len(f) == 0 {
		return true, h.mcpList()
	}
	switch f[0] {
	case "on", "off":
		if len(f) < 2 {
			return true, "Usage: /mcp on|off <mcp_id>"
		}
		disabled := f[0] == "off"
		return h.commitMCP(f[1], func(m *config.MCPDef) { m.Disabled = disabled },
			fmt.Sprintf("disabled=%t", disabled))
	case "on-tool", "off-tool":
		if len(f) < 3 {
			return true, "Usage: /mcp on-tool|off-tool <mcp_id> <tool_name>"
		}
		add := f[0] == "off-tool"
		return h.commitMCP(f[1], func(m *config.MCPDef) {
			m.DisabledTools = toggleMember(m.DisabledTools, f[2], add)
		}, f[0]+" "+f[2])
	default:
		return true, "Usage:\n  /mcp\n  /mcp on|off <mcp_id>\n  /mcp on-tool|off-tool <mcp_id> <tool_name>"
	}
}

// mcpList renders the MCP pool with each member's toggle state.
func (h *ChatHandler) mcpList() string {
	h.App.Registry.Mu.RLock()
	defer h.App.Registry.Mu.RUnlock()
	if len(h.App.Registry.MCPs) == 0 {
		return "No MCPs registered."
	}
	ids := make([]string, 0, len(h.App.Registry.MCPs))
	for id := range h.App.Registry.MCPs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var lines []string
	for _, id := range ids {
		m := h.App.Registry.MCPs[id]
		state := "on"
		if m.Disabled {
			state = "OFF"
		}
		line := fmt.Sprintf("  %s [%s] tools=%d", id, state, len(m.AvailableTools))
		if len(m.DisabledTools) > 0 {
			line += " disabled_tools=" + strings.Join(m.DisabledTools, ",")
		}
		lines = append(lines, line)
	}
	return fmt.Sprintf("MCPs (%d):\n%s", len(ids), strings.Join(lines, "\n"))
}

// commitMCP applies mutate to a COPY of the MCP definition and persists it via
// the web UI's whole-replace path. Mutating a copy keeps the live registry
// untouched until the commit reloads it, so a failed marshal changes nothing.
func (h *ChatHandler) commitMCP(mcpID string, mutate func(*config.MCPDef), what string) (bool, string) {
	def := h.App.Registry.GetMCP(mcpID)
	if def == nil {
		return true, fmt.Sprintf("MCP %q not found.", mcpID)
	}
	cp := *def
	mutate(&cp)
	data, err := json.Marshal(&cp)
	if err != nil {
		return true, fmt.Sprintf("Error marshaling MCP %q: %v", mcpID, err)
	}
	ops := []config.PatchOp{{Op: config.OpAdd, Path: "/mcps/" + mcpID, Value: json.RawMessage(data)}}
	if err := h.App.Registry.CommitOps(ops, "chat_user", ""); err != nil {
		return true, fmt.Sprintf("Error saving MCP %q: %v", mcpID, err)
	}
	return true, fmt.Sprintf("✅ MCP %q updated (%s).", mcpID, what)
}

// toggleMember returns list with name added (add=true) or removed (add=false),
// preserving order and de-duplicating.
func toggleMember(list []string, name string, add bool) []string {
	out := make([]string, 0, len(list)+1)
	for _, v := range list {
		if v != name {
			out = append(out, v)
		}
	}
	if add {
		out = append(out, name)
	}
	return out
}

// cmdDecision handles "/decision <task_id> <decision_id> <choice> [output]".
func (h *ChatHandler) cmdDecision(arg string) (bool, string) {
	args := strings.Fields(arg)
	if len(args) < 3 {
		return true, "Usage: /decision <task_id> <decision_id> <choice> [output_json]\n" +
			"Choices: skip | abort | retry | refine_and_retry | fulfill (needs output_json)\n" +
			"Example: /decision task_abc dec_123 retry"
	}
	if h.App == nil || h.App.Scheduler == nil {
		return true, "Error: scheduler not available."
	}
	taskID, decisionID, choice := args[0], args[1], args[2]
	output := ""
	if len(args) > 3 {
		output = strings.Join(args[3:], " ")
	}
	if h.humanApprovalDecisionDenied(taskID, decisionID) {
		return true, "Denied: human_approval_required decisions may only be approved in the web UI, not via Telegram /decision."
	}
	// Same routing table as orchestrator_submit_decision (register O42): this
	// command advertised `fulfill` and then forwarded every choice, payload
	// included, to SubmitDecisionWithPayload — which has no fulfill branch, so
	// the advertised answer came back "unknown choice" and a delegation hand-back
	// could not be sent from Telegram at all.
	toFulfill, outputIgnored := tools.ResolveDecision(h.App.Scheduler, taskID, decisionID, choice, output)
	if toFulfill {
		if output == "" {
			return true, "Error: choice=\"fulfill\" needs the result JSON as the 4th argument: /decision <task_id> <decision_id> fulfill {\"json\":\"here\"}"
		}
		if err := h.App.Scheduler.FulfillStep(taskID, decisionID, output); err != nil {
			return true, fmt.Sprintf("Decision failed: %v", err)
		}
		return true, fmt.Sprintf("✅ Delegation fulfilled: task=%s decision=%s", taskID, decisionID)
	}
	if err := h.App.Scheduler.SubmitDecisionWithPayload(taskID, decisionID, choice, output); err != nil {
		return true, fmt.Sprintf("Decision failed: %v", err)
	}
	if outputIgnored {
		return true, fmt.Sprintf("✅ Decision submitted: task=%s decision=%s choice=%s (output ignored: this decision type takes no payload)", taskID, decisionID, choice)
	}
	return true, fmt.Sprintf("✅ Decision submitted: task=%s decision=%s choice=%s", taskID, decisionID, choice)
}

// cmdApprove handles "/approve <task_id> <decision_id>" — submits "retry".
func (h *ChatHandler) cmdApprove(arg string) (bool, string) {
	args := strings.Fields(arg)
	if len(args) < 2 {
		return true, "Usage: /approve <task_id> <decision_id>\n" +
			"Submits 'retry' choice to approve the pending decision."
	}
	if h.App == nil || h.App.Scheduler == nil {
		return true, "Error: scheduler not available."
	}
	taskID, decisionID := args[0], args[1]
	if h.humanApprovalDecisionDenied(taskID, decisionID) {
		return true, "Denied: human_approval_required decisions may only be approved in the web UI, not via Telegram /approve."
	}
	if err := h.App.Scheduler.SubmitDecision(taskID, decisionID, "retry"); err != nil {
		return true, fmt.Sprintf("Approve failed: %v", err)
	}
	return true, fmt.Sprintf("✅ Approved: task=%s decision=%s (choice=retry)", taskID, decisionID)
}

func (h *ChatHandler) handleSlashCommand(content string, session *chat.ChatSession, sessionID string, userTier string) (handled bool, response string) {
	if !strings.HasPrefix(content, "/") {
		return false, ""
	}

	parts := strings.SplitN(content, " ", 2)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}

	// ── Admin Command Gate ──────────────────────────────────────────
	// Commands like /cancel, /decision, /approve, /config require admin
	// tier. Non-admin users (guest, authorized) get a rejection message.
	if IsAdminCommand(cmd) && userTier != TierAdmin {
		return true, "🚫 This command requires admin access. Your tier: " + userTier
	}

	switch cmd {
	case "/help":
		return true, slashHelpText(userTier)

	case "/tools":
		var names []string
		for _, td := range h.Harness.ToolDefinitions() {
			names = append(names, td.Name)
		}
		return true, fmt.Sprintf("Tools available in this session: %s", strings.Join(names, ", "))

	case "/mcp":
		return h.cmdMCP(arg)

	case "/delegate":
		if arg == "" {
			return true, "Usage: /delegate <prompt>\nExample: /delegate analyze the data in outputs/report.json and summarize key findings"
		}
		if h.Harness.Delegate == nil {
			return true, "Error: delegate is not configured in this session."
		}
		taskID, err := h.Harness.Delegate(arg)
		if err != nil {
			return true, fmt.Sprintf("Delegate failed: %v", err)
		}
		return true, fmt.Sprintf("Delegated to orchestrator task %s. Use /status %s to track progress.", taskID, taskID)

	case "/clear":
		// Two-step confirmation: /clear alone shows the session ID and asks
		// for confirmation. /clear <sessionID> confirm executes the clear.
		// This prevents accidental irreversible history loss.
		expectedID := sessionID
		now := time.Now().Unix()
		if arg == "" {
			// Step 1: issue confirmation challenge.
			h.clearConfirmations.Store(sessionID, now+30)
			return true, fmt.Sprintf(
				"⚠️ This will IRREVERSIBLY delete all message history for session %s.\n"+
					"To confirm, send: /clear %s confirm\n"+
					"(confirmation expires in 30 seconds)", expectedID, expectedID)
		}
		// Step 2: verify confirmation.
		parts := strings.Fields(arg)
		if len(parts) < 2 || parts[0] != expectedID || parts[1] != "confirm" {
			return true, fmt.Sprintf(
				"❌ Clear cancelled. Usage: /clear %s confirm\n"+
					"(session ID must match the current session)", expectedID)
		}
		val, ok := h.clearConfirmations.LoadAndDelete(sessionID)
		if !ok {
			return true, "❌ No pending confirmation. Start with /clear first."
		}
		deadline := val.(int64)
		if now > deadline {
			return true, "❌ Confirmation expired. Start with /clear again."
		}
		session.Clear()
		// audit fix L4: surface Persist failure — previously swallowed, so
		// /clear reported success but the on-disk session retained all messages
		// and a restart restored the "cleared" history.
		if perr := h.Store.Persist(session); perr != nil {
			return true, fmt.Sprintf("⚠️ Session %s cleared in memory, but failed to persist: %v", expectedID, perr)
		}
		// audit MEDIUM (2026-10-28): clear lastThinking to prevent unbounded
		// memory growth — sync.Map had Store but no Delete on session clear.
		h.lastThinking.Delete(sessionID)
		// audit H2/H3/H4 (2026-11-05): clear per-session dedup maps so cleared
		// sessions don't retain followUpSeen/wakeSeen entries forever.
		h.cleanupSessionChatState(sessionID)
		return true, fmt.Sprintf("✅ Session %s history cleared.", expectedID)

	case "/forget":
		return h.handleForget(arg, sessionID)

	case "/stop":
		if oldCancel, ok := h.cancelFuncs.LoadAndDelete(sessionID); ok {
			oldCancel.(context.CancelFunc)()
			return true, "⏹ Stopped the current run for this session."
		}
		return true, "No active run to stop."

	case "/process":
		return h.cmdProcess(arg, sessionID)

	case "/thinking":
		thinking := h.GetLastThinking(sessionID)
		if thinking == "" {
			return true, "No thinking/reasoning content available for this session."
		}
		// Truncate to Telegram's 4096 char limit (leave room for header).
		header := "💭 Full thinking (" + strconv.Itoa(utf8.RuneCountInString(thinking)) + " chars):\n"
		maxBody := tgMaxMsgLen - utf8.RuneCountInString(header) - 20
		if maxBody < 0 {
			maxBody = 1000
		}
		return true, header + truncateRunes(thinking, maxBody)

	case "/pending":
		if h.App == nil || h.App.Scheduler == nil {
			return true, "Error: scheduler not available."
		}
		if arg == "" {
			// List all tasks with pending decisions.
			graphs := h.App.Scheduler.ListGraphs()
			var lines []string
			for _, g := range graphs {
				// Do NOT filter on graph status == "blocked": addDecision
				// deliberately keeps the graph "running" (only the step
				// blocks — see scheduler_decision_gate.go). Pending
				// decisions are the authoritative signal.
				taskID, _ := g["task_id"].(string)
				// Get full status to see pending decisions.
				full, ok := h.App.Scheduler.GetStatus(taskID, "full")
				if !ok {
					continue
				}
				decisions, _ := full["pending_decisions"].([]map[string]any)
				if len(decisions) == 0 {
					continue
				}
				for _, d := range decisions {
					did, _ := d["id"].(string)
					dtype, _ := d["type"].(string)
					stepID, _ := d["step_id"].(string)
					lines = append(lines, fmt.Sprintf("  task=%s decision=%s step=%s type=%s", taskID, did, stepID, dtype))
				}
			}
			if len(lines) == 0 {
				return true, "No pending decisions."
			}
			return true, fmt.Sprintf("Pending decisions (%d):\n%s", len(lines), strings.Join(lines, "\n"))
		}
		// Show pending decisions for a specific task.
		status, ok := h.App.Scheduler.GetStatus(arg, "full")
		if !ok {
			return true, fmt.Sprintf("Task %q not found.", arg)
		}
		decisions, _ := status["pending_decisions"].([]map[string]any)
		if len(decisions) == 0 {
			return true, fmt.Sprintf("No pending decisions for task %s.", arg)
		}
		var lines []string
		for _, d := range decisions {
			did, _ := d["id"].(string)
			dtype := fmt.Sprint(d["type"]) // schemas.DecisionType; .(string) silently fails
			stepID, _ := d["step_id"].(string)
			options, _ := d["options"].([]string)
			lines = append(lines, fmt.Sprintf("  decision_id=%s\n    step=%s type=%s\n    options: %s", did, stepID, dtype, strings.Join(options, ", ")))
		}
		return true, fmt.Sprintf("Pending decisions for task %s (%d):\n%s", arg, len(decisions), strings.Join(lines, "\n"))

	case "/decision":
		return h.cmdDecision(arg)

	case "/approve":
		return h.cmdApprove(arg)

	case "/role":
		return h.handleRoleCommand(arg, session)

	case "/tasks":
		if h.App == nil || h.App.Scheduler == nil {
			return true, "Error: scheduler not available."
		}
		return true, formatTasksList(h.App.Scheduler.ListGraphs())

	case "/status":
		if arg == "" {
			return true, "Usage: /status <task_id>"
		}
		if h.App == nil || h.App.Scheduler == nil {
			return true, "Error: scheduler not available."
		}
		status, ok := h.App.Scheduler.GetStatus(arg, "summary")
		if !ok {
			return true, fmt.Sprintf("Task %q not found.", arg)
		}
		data, _ := json.MarshalIndent(status, "", "  ")
		return true, string(data)

	case "/cancel":
		if arg == "" {
			return true, "Usage: /cancel <task_id>"
		}
		if h.App == nil || h.App.Scheduler == nil {
			return true, "Error: scheduler not available."
		}
		if err := h.App.Scheduler.CancelTask(arg); err != nil {
			return true, fmt.Sprintf("Cancel failed: %v", err)
		}
		return true, fmt.Sprintf("✅ Task %s cancelled.", arg)

	case "/logs":
		if arg == "" {
			return true, "Usage: /logs <task_id>"
		}
		return true, h.handleTaskLogTrace(arg, "Logs")

	case "/trace":
		return true, h.handleTaskLogTrace(sessionID, "Trace")

	case "/models":
		if h.App == nil || h.App.Registry == nil {
			return true, "Error: registry not available."
		}
		models := h.App.Registry.GetHealthyModelsSummary()
		if len(models) == 0 {
			return true, "No models configured."
		}
		var lines []string
		for _, m := range models {
			id, _ := m["model"].(string)
			provider, _ := m["provider"].(string)
			healthy, _ := m["healthy"].(bool)
			mark := "✅"
			if !healthy {
				mark = "❌"
			}
			lines = append(lines, fmt.Sprintf("  %s %s (%s)", mark, id, provider))
		}
		return true, "Available models:\n" + strings.Join(lines, "\n")

	case "/skills":
		if h.App == nil || h.App.Registry == nil {
			return true, "Error: registry not available."
		}
		h.App.Registry.Mu.RLock()
		var lines []string
		for id, skill := range h.App.Registry.Skills {
			name := skill.Name
			if name == "" {
				name = id
			}
			cap := skill.Capability
			if cap == "" {
				cap = "general"
			}
			lines = append(lines, fmt.Sprintf("  %s — %s [%s]", id, name, cap))
		}
		h.App.Registry.Mu.RUnlock()
		sort.Strings(lines)
		if len(lines) == 0 {
			return true, "No skills registered."
		}
		return true, fmt.Sprintf("Registered skills (%d):\n%s", len(lines), strings.Join(lines, "\n"))

	case "/schedule":
		if h.App == nil || h.App.SStore == nil {
			return true, "Error: schedule store not available."
		}
		schedules := h.App.SStore.List()
		if len(schedules) == 0 {
			return true, "No scheduled tasks."
		}
		var lines []string
		for _, s := range schedules {
			lines = append(lines, fmt.Sprintf("  %s — %s [%s] %s", s.ID, s.Name, s.Type, s.Status))
		}
		return true, fmt.Sprintf("Scheduled tasks (%d):\n%s", len(schedules), strings.Join(lines, "\n"))

	case "/discover":
		if h.App == nil || h.App.Navigator == nil || h.App.Registry == nil {
			return true, "Error: capability navigator not available."
		}
		menu := h.App.Navigator.GenerateDynamicMenu(h.App.Registry)
		return true, menu

	case "/config":
		if h.App == nil || h.App.Registry == nil {
			return true, "Error: registry not available."
		}
		raw, err := h.App.Registry.LoadRaw()
		if err != nil {
			return true, fmt.Sprintf("Error loading config: %v", err)
		}
		// Pretty-print if it's valid JSON, otherwise return as-is.
		var pretty map[string]any
		if json.Unmarshal(raw, &pretty) == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			raw = out
		}
		s := string(raw)
		// Truncate to 3000 chars to stay within Telegram's 4096 limit.
		if len(s) > 3000 {
			s = s[:3000] + "\n... (truncated)"
		}
		return true, s

	case "/run":
		return h.cmdRun(arg)

	default:
		// Unknown slash command — don't intercept, let the LLM handle it
		// (the model might interpret it as a literal request).
		return false, ""
	}
}

// cmdProcess implements the /process slash command — toggles per-session
// process-info visibility (thinking, tool calls, trajectory events).
// Not admin-only: any user can toggle their own session. (2026-10-27)
func (h *ChatHandler) cmdProcess(arg, sessionID string) (bool, string) {
	switch arg {
	case "":
		if h.IsProcessInfoDisabled(sessionID) {
			return true, "Process info: OFF (quiet mode).\nThinking, tool calls, and trajectory events are hidden.\nUse /process on to re-enable."
		}
		return true, "Process info: ON (default).\nThinking, tool calls, and trajectory events are shown.\nUse /process off to hide them."
	case "on", "true", "1", "yes":
		h.SetProcessInfoDisabled(sessionID, false)
		return true, "✅ Process info enabled. You will see thinking, tool calls, and trajectory events."
	case "off", "false", "0", "no":
		h.SetProcessInfoDisabled(sessionID, true)
		return true, "🔇 Process info disabled (quiet mode).\nOnly the final response and errors will be shown.\nThe typing indicator will still appear while working."
	default:
		return true, "Usage: /process [on|off]\n  /process on  — show process info (default)\n  /process off — hide process info (quiet mode)\n  /process     — show current state"
	}
}

// cmdRun implements the /run slash command — direct shell execution from
// the chat frontend, mirroring orchestrator_run_command. Requires BOTH
// WebPowerMode config flag AND admin tier. (2026-10-27)
func (h *ChatHandler) cmdRun(arg string) (bool, string) {
	if h.App == nil || h.App.Registry == nil {
		return true, "Error: registry not available."
	}
	if !h.App.Registry.System.FeatureFlags.WebPowerMode {
		return true, "🚫 /run requires web_power_mode=true in config.json. The chat frontend is in standard (non-power) mode."
	}
	if arg == "" {
		return true, "Usage: /run <command> [args...]\nExample: /run git status"
	}
	tokens := strings.Fields(arg)
	if len(tokens) == 0 {
		return true, "Usage: /run <command> [args...]"
	}
	cmdName := tokens[0]
	cmdArgs := tokens[1:]
	exec := core.NewControlledExecutor()
	exec.IdleTimeout = 15 * time.Second
	exec.TotalTimeout = 120 * time.Second
	res, err := exec.Run(context.Background(), cmdName, cmdArgs, "", nil)
	if err != nil {
		return true, fmt.Sprintf("exec error: %v", err)
	}
	out := string(res.Stdout)
	if len(out) > 2000 {
		out = out[:2000] + "\n... (truncated)"
	}
	if len(res.Stderr) > 0 {
		stderr := string(res.Stderr)
		if len(stderr) > 1000 {
			stderr = stderr[:1000] + "\n... (truncated)"
		}
		return true, fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, out, stderr)
	}
	return true, fmt.Sprintf("exit=%d\n%s", res.ExitCode, out)
}

// handleTaskLogTrace returns formatted task log/trace events for a session.
// label is "Logs" or "Trace" — used in the output header and error messages.
func (h *ChatHandler) handleTaskLogTrace(taskID, label string) string {
	if h.App == nil || h.App.Logger == nil {
		return "Error: logger not available."
	}
	events, err := h.App.Logger.ReadTaskLogs(taskID)
	if err != nil {
		return fmt.Sprintf("Error reading %s: %v", label, err)
	}
	if len(events) == 0 {
		return fmt.Sprintf("No %s events for %s.", label, taskID)
	}
	if len(events) > 20 {
		events = events[len(events)-20:]
	}
	var lines []string
	for _, ev := range events {
		evType, _ := ev["event"].(string)
		msg, _ := ev["detail"].(string)
		ts, _ := ev["timestamp"].(string)
		lines = append(lines, fmt.Sprintf("  [%s] %s: %s", ts, evType, msg))
	}
	return fmt.Sprintf("%s for %s (last %d):\n%s", label, taskID, len(events), strings.Join(lines, "\n"))
}

// slashHelpText returns the help text for all available slash commands.
// Commands are split into Public and Admin sections. Admin commands are
// shown with a lock/unlock indicator based on the caller's tier.
func slashHelpText(userTier string) string {
	tierLabel := userTier
	if tierLabel == "" {
		tierLabel = TierGuest
	}
	adminHeader := "Admin commands (🔒 locked — requires admin tier):"
	if userTier == TierAdmin {
		adminHeader = "Admin commands (🔑 unlocked):"
	}
	return fmt.Sprintf("Available commands (your tier: %s):\n\n", tierLabel) +
		"Public commands:\n" +
		"  /help               — show this help\n" +
		"  /tools              — list LLM tools in this session\n" +
		"  /delegate <prompt>  — delegate to orchestrator (skips LLM, full MCP)\n" +
		"  /role [id]          — list roles or switch role\n" +
		"  /clear              — clear session history (in-session only)\n" +
		"  /forget <scope>     — delete cross-session memory (irreversible, see /forget)\n" +
		"  /stop               — stop the current run in this session\n" +
		"  /thinking           — show full thinking/reasoning from last response\n" +
		"  /process [on|off]   — toggle process info (thinking, tool calls, trajectory)\n" +
		"\n" +
		"Task management:\n" +
		"  /tasks              — list recent tasks\n" +
		"  /status <task_id>   — get task status\n" +
		"  /logs <task_id>     — get task execution logs\n" +
		"  /trace              — show this session's scheduling trajectory\n" +
		"\n" +
		"Decision & approval:\n" +
		"  /pending [task_id]  — list pending decisions (all or for a task)\n" +
		"  /decision <task> <decision> <choice> — submit a decision\n" +
		"  /approve <task> <decision>          — approve (shortcut for retry)\n" +
		"\n" +
		"System info:\n" +
		"  /models             — list available AI models\n" +
		"  /skills             — list registered skills\n" +
		"  /schedule           — list scheduled tasks\n" +
		"  /discover           — discover capabilities (roles, skills, MCPs)\n" +
		"\n" +
		adminHeader + "\n" +
		"  /mcp                — list MCPs / toggle pool state\n" +
		"  /cancel <task_id>   — cancel a running task\n" +
		"  /config             — show current config (secrets masked)\n" +
		"  /run <cmd> [args]   — direct shell execution (requires web_power_mode=true)\n" +
		"\n" +
		"Any other input is sent to the chat LLM as normal."
}

// DeleteTaskArtifacts removes the entire outputs/<taskID>/ directory tree.
// Rejects if taskID contains path separators or traversal sequences (safety).
// See docs/plan/SESSION_MEMORY_DELETION_DESIGN.md §Storage Layer Changes.
func DeleteTaskArtifacts(root, taskID string) error {
	if taskID == "" || strings.ContainsAny(taskID, "/\\") || strings.Contains(taskID, "..") {
		return fmt.Errorf("invalid taskID: %q", taskID)
	}
	return os.RemoveAll(filepath.Join(root, taskID))
}

// forgetChallengeKey builds the confirmation map key for /forget.
// Includes scope + args so independent challenges can coexist.
func forgetChallengeKey(sessionID, scope, args string) string {
	return sessionID + "|forget|" + scope + "|" + args
}

// handleForget implements the /forget slash command.
// Two-step confirmation with 60s window (2× /clear).
//
// Scopes:
//
//	/forget task <taskID> confirm  — delete ContextArchive entries + artifacts, mark ledger row aborted
//	/forget user confirm           — delete MemoryBank user-scoped entries
//	/forget memory <query> confirm — search-and-delete MemoryBank entries
//
// See docs/plan/SESSION_MEMORY_DELETION_DESIGN.md.
func (h *ChatHandler) handleForget(arg, sessionID string) (bool, string) {
	const forgetWindow = 60 // seconds — 2× /clear because irreversible

	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return true, forgetUsageText()
	}
	scope := fields[0]

	// Parse scope-specific args.
	var scopeArgs string
	switch scope {
	case "task":
		if len(fields) < 2 {
			return true, "Usage: /forget task <taskID> confirm\nExample: /forget task abc123 confirm"
		}
		scopeArgs = fields[1]
	case "user":
		scopeArgs = ""
	case "memory":
		if len(fields) < 2 {
			return true, "Usage: /forget memory <query> confirm\nExample: /forget memory \"old project\" confirm"
		}
		scopeArgs = strings.Join(fields[1:], " ")
	default:
		return true, forgetUsageText()
	}

	// Check if this is the confirm step.
	isConfirm := len(fields) > 0 && fields[len(fields)-1] == "confirm"
	// For task/memory, confirm is the LAST field after scope+args.
	// Re-parse: if the last field is "confirm", strip it.
	if isConfirm {
		if scope == "task" && len(fields) >= 3 && fields[2] == "confirm" {
			// /forget task <taskID> confirm
		} else if scope == "user" && len(fields) == 2 && fields[1] == "confirm" {
			// /forget user confirm
		} else if scope == "memory" && fields[len(fields)-1] == "confirm" {
			scopeArgs = strings.Join(fields[1:len(fields)-1], " ")
		} else {
			isConfirm = false
		}
	}

	key := forgetChallengeKey(sessionID, scope, scopeArgs)
	now := time.Now().Unix()

	if !isConfirm {
		// Step 1: issue confirmation challenge with live counts.
		h.forgetConfirmations.Store(key, now+forgetWindow)
		preview := h.forgetPreview(scope, scopeArgs)
		return true, fmt.Sprintf(
			"⚠️ /forget %s %s will delete:\n%s\n"+
				"Type: /forget %s %s confirm\n"+
				"Expires in %d seconds.",
			scope, scopeArgs, preview, scope, scopeArgs, forgetWindow)
	}

	// Step 2: verify confirmation.
	val, ok := h.forgetConfirmations.LoadAndDelete(key)
	if !ok {
		return true, "❌ No pending confirmation. Start with /forget first."
	}
	deadline := val.(int64)
	if now > deadline {
		return true, "❌ Confirmation expired. Start with /forget again."
	}

	// Execute deletion.
	result := h.forgetExecute(scope, scopeArgs)
	log.Printf("[AUDIT] /forget scope=%s args=%q session=%s result=%s", scope, scopeArgs, sessionID, result)
	return true, result
}

// forgetPreview returns a human-readable summary of what will be deleted.
func (h *ChatHandler) forgetPreview(scope, scopeArgs string) string {
	switch scope {
	case "task":
		var lines []string
		if h.Harness != nil && h.Harness.Archive != nil {
			count := h.countArchiveByTask(scopeArgs)
			lines = append(lines, fmt.Sprintf("  - %d ContextArchive entries for task %s", count, scopeArgs))
		}
		lines = append(lines, "  - Artifact outputs at outputs/"+scopeArgs+"/ (includes manifest.json, the only resume source)")
		lines = append(lines, "  - Ledger row for task "+scopeArgs+" → status 'aborted'")
		return strings.Join(lines, "\n")
	case "user":
		return "  - MemoryBank user-scoped entries (user_profile)"
	case "memory":
		return "  - MemoryBank entries matching \"" + scopeArgs + "\""
	}
	return ""
}

// countArchiveByTask counts ContextArchive items matching a taskID (read-only).
func (h *ChatHandler) countArchiveByTask(taskID string) int {
	if h.Harness == nil || h.Harness.Archive == nil {
		return 0
	}
	h.Harness.Archive.Load()
	// Use Search with empty query to get all, then filter — or just count
	// via the internal items. Since we can't access private fields from here,
	// use SearchRecentStepSummaries with a large N as a proxy count.
	items := h.Harness.Archive.SearchRecentStepSummaries(taskID, h.Harness.ProjectOf(taskID), 10000)
	return len(items)
}

// forgetExecute performs the actual deletion and returns a result message.
func (h *ChatHandler) forgetExecute(scope, scopeArgs string) string {
	switch scope {
	case "task":
		var parts []string
		if h.Harness != nil && h.Harness.Archive != nil {
			deleted, err := h.Harness.Archive.DeleteByScope(core.ArchiveScope{TaskID: scopeArgs})
			if err != nil {
				return fmt.Sprintf("❌ Archive deletion failed: %v", err)
			}
			parts = append(parts, fmt.Sprintf("%d archive entries", deleted))
		}
		if h.Harness != nil && h.Harness.OutputBase != "" {
			if err := DeleteTaskArtifacts(filepath.Join(h.Harness.OutputBase), scopeArgs); err != nil {
				parts = append(parts, fmt.Sprintf("artifact cleanup error: %v", err))
			} else {
				parts = append(parts, "artifact outputs")
			}
		}
		// The artifacts dir holds manifest.json, so the deletion above removed the
		// only resume source; without a terminal ledger row the task is reported
		// as running forever (O32).
		if h.App != nil && h.App.Scheduler != nil {
			if err := h.App.Scheduler.MarkTaskAborted(scopeArgs); err != nil {
				parts = append(parts, fmt.Sprintf("ledger error: %v", err))
			} else {
				parts = append(parts, "ledger row marked aborted")
			}
		}
		return "✅ Deleted: " + strings.Join(parts, " + ") + "."

	case "user":
		if h.Harness == nil || h.Harness.MemoryBank == nil {
			return "❌ MemoryBank not available in this session."
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleted, err := h.Harness.MemoryBank.DeleteUserScope(ctx)
		if err != nil {
			return fmt.Sprintf("❌ MemoryBank deletion failed: %v", err)
		}
		return fmt.Sprintf("✅ Deleted %d MemoryBank user-scoped entries.", deleted)

	case "memory":
		if h.Harness == nil || h.Harness.MemoryBank == nil {
			return "❌ MemoryBank not available in this session."
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		deleted, err := h.Harness.MemoryBank.SearchAndDelete(ctx, scopeArgs)
		if err != nil {
			return fmt.Sprintf("❌ MemoryBank search-and-delete failed: %v", err)
		}
		return fmt.Sprintf("✅ Deleted %d MemoryBank entries matching %q.", deleted, scopeArgs)
	}
	return "❌ Unknown scope."
}

// forgetUsageText returns the help text for /forget.
func forgetUsageText() string {
	return "Usage: /forget <scope> [args] confirm\n\n" +
		"Scopes:\n" +
		"  /forget task <taskID> confirm  — delete archive entries + artifacts, mark ledger row aborted\n" +
		"  /forget user confirm           — delete MemoryBank user-scoped entries\n" +
		"  /forget memory <query> confirm — delete MemoryBank entries matching query\n\n" +
		"⚠️ Irreversible. 60-second confirmation window.\n" +
		"Session memory persists across /clear. Use /forget for cross-session deletion."
}

// handleRoleCommand handles the /role slash command.
func (h *ChatHandler) handleRoleCommand(arg string, session *chat.ChatSession) (bool, string) {
	if arg == "" {
		// List available roles
		h.App.Registry.Mu.RLock()
		var lines []string
		for id, role := range h.App.Registry.Roles {
			name := role.Name
			if name == "" {
				name = id
			}
			lines = append(lines, fmt.Sprintf("  %s — %s", id, name))
		}
		h.App.Registry.Mu.RUnlock()
		sort.Strings(lines)
		active := session.GetActiveRoleID()
		if active == "" {
			active = "default"
		}
		return true, fmt.Sprintf("Active role: %s\n\nAvailable roles:\n%s\n\nUse /role <id> to switch. Use /role default to reset.", active, strings.Join(lines, "\n"))
	}
	if arg == "default" {
		session.SetActiveRoleID("")
		return true, "Switched to default assistant (no role-specific persona)."
	}
	h.App.Registry.Mu.RLock()
	role := h.App.Registry.Roles[arg]
	if role == nil {
		h.App.Registry.Mu.RUnlock()
		return true, fmt.Sprintf("Role %q not found. Use /role to list available roles.", arg)
	}
	// audit LOGIC-4: copy ALL fields under lock to avoid race with hot-reload
	name, model, provider, purpose, bestFor := role.Name, role.Model, role.Provider, role.Purpose, role.BestFor
	h.App.Registry.Mu.RUnlock()
	// Persist the role switch — the next message will pick it up from the session
	session.SetActiveRoleID(arg)
	info := fmt.Sprintf("✅ Switched to role: %s (%s)\nModel: %s\nProvider: %s", arg, name, model, provider)
	if purpose != "" {
		info += "\nPurpose: " + purpose
	}
	if bestFor != "" {
		info += "\nBest for: " + bestFor
	}
	return true, info
}

// cmdLive implements the /live slash command for Live Co-pilot shared sessions.
// Subcommands:
//
//	/live start  — mark current session as shared (admin only)
//	/live join <id> — look up a shared session by ID
//	/live end    — stop sharing the current session (admin only)
//	/live status — show shared session info
//
// See docs/plan/LIVE_COPILOT_SHARED_SESSION_DESIGN.md §2.3.
func (h *ChatHandler) cmdLive(content string, session *chat.ChatSession, sessionID string, userTier string, callerID string) (bool, string) {
	parts := strings.SplitN(content, " ", 3)
	subcmd := ""
	if len(parts) > 1 {
		subcmd = parts[1]
	}

	switch subcmd {
	case "start":
		if userTier != TierAdmin {
			return true, "🚫 /live start requires admin access. Your tier: " + userTier
		}
		ownerID := callerID
		if ownerID == "" {
			ownerID = "admin" // fallback when no caller identity is available
		}
		session.SetShared(true)
		session.SetOwnerID(ownerID)
		return true, fmt.Sprintf("✅ Session %s is now shared.\nOwner: %s\nPublic users can join by sending messages with session_id=%s.\nTheir requests will be routed through your approval pipeline.", sessionID, ownerID, sessionID)

	case "join":
		if len(parts) < 3 || parts[2] == "" {
			return true, "Usage: /live join <session-id>\nExample: /live join abc-123-def"
		}
		targetID := strings.TrimSpace(parts[2])
		target := h.Store.Get(targetID)
		if target == nil {
			return true, "❌ Session not found: " + targetID
		}
		if !target.IsShared() {
			return true, "❌ Session is not shared: " + targetID
		}
		return true, fmt.Sprintf("✅ Session %s is shared (owner: %s).\nTo join, send your next message with session_id=%s.\nYour requests will be routed through the owner's approval pipeline.", targetID, target.GetOwnerID(), targetID)

	case "end":
		if userTier != TierAdmin {
			return true, "🚫 /live end requires admin access. Your tier: " + userTier
		}
		if !session.IsShared() {
			return true, "This session is not shared."
		}
		session.SetShared(false)
		return true, "✅ Shared session ended. New messages will no longer be routed through the owner's approval pipeline."

	case "status":
		if !session.IsShared() {
			return true, "This session is not shared.\nUse /live start to share it (admin only)."
		}
		return true, fmt.Sprintf("📡 Shared session: %s\nOwner: %s\nGuest requests are routed through the owner's approval pipeline.\nUse /live end to stop sharing (admin only).", sessionID, session.GetOwnerID())

	default:
		return true, "Usage: /live [start|join|end|status]\n" +
			"  /live start        — mark this session as shared (admin)\n" +
			"  /live join <id>    — look up a shared session by ID\n" +
			"  /live end          — stop sharing this session (admin)\n" +
			"  /live status       — show shared session info"
	}
}
