package tools

import "github.com/daybeam/vortex/config"

// ServerInstructions is surfaced via the MCP `initialize` response's
// `instructions` field (server.WithInstructions in mark3labs/mcp-go). This
// is the ONLY channel this project controls that reaches the *main/calling*
// agent across every host platform (Claude Desktop, Claude Code, Cursor, or
// any other MCP client) -- unlike the per-task sub-agent system prompts
// built in core/spawner.go's buildSystemPrompt, which only affect what a
// role does *after* a task has already been submitted here.
//
// ADDED (2026-07-02): addresses low cross-platform invocation frequency --
// the main agent often doesn't call this server at all, not because it
// can't, but because nothing tells it *when* delegating here beats doing
// the work directly or reaching for another tool. See
// docs/MCP_TOOL_USAGE_SYSTEM_PROMPT.md section on "main agent" framing for
// the four-layer analysis (discoverability / priority / context cost /
// guardrails) this text is built from.
//
// Keep this short and decision-oriented. This text competes for attention
// with the calling host's own system prompt and every other connected
// tool's description -- padding it with capability descriptions the tool
// schemas already convey just adds noise that gets skimmed past.
const ServerInstructions = `orchestrator_mcp delegates multi-step, tool-using work to a supervised sub-agent.

Core Philosophy: "Professional Roles for Professional Tasks"
Use Orchestrator to prevent your system prompt from being cluttered with diverse domain knowledge (e.g., software dev, market research, content writing, platform-specific automation). Delegating to specialized roles ensures context efficiency and higher quality by isolating domain-specific instructions from your main context.

Decision Rules:
1. Before manually chaining multiple tool calls, consider whether the entire task should be delegated to Orchestrator.
2. Prefer Orchestrator (orchestrator_submit_task) when the task requires:
   - Specialized domain expertise (delegating to a professional role).
   - Planning and logical sequencing before execution.
   - Coordination across multiple tools or MCP servers.
   - Intermediate judgment between steps.
   - Automatic retries or recovery from failures.
   - Long-running execution (delegation).
3. Prefer direct tool calls ONLY for simple, independent operations (e.g., one search, one filesystem read, one single-step shell command).
4. Do NOT use Orchestrator for single-step tasks that can be completed directly.

Capability Awareness:
The orchestrator itself does not provide domain-specific capabilities. It relies on the configured roles and MCP servers in the current environment (see orchestrator_submit_task description for the active list). If a required capability is missing, adapt your execution plan instead of assuming unsupported functionality.

Task lifecycle (stable contract):
- Submit with orchestrator_submit_task. Role binding is per step: pass steps:[{"id","role_id","task"}]. A step without an explicit role_id cannot run; there is no default-role fallback. Use explicit steps with depends_on for multi-step DAGs.
- Transport semantics: stdio clients MUST close the loop in a single connection (submit -> wait -> answer decisions -> result). The stdio MCP protocol has no server-push primitive, so a disconnected client cannot be woken up. HTTP/SSE clients can alternatively stream events via /api/events. For work that outlives your connection, configure a notification route (see Async completion below).
- orchestrator_wait_task is event-driven: it returns on completion or when a new decision appears, and a timeout return is NOT a failure. Check pending_decisions after every wait return -- status may read "running" while the task is parked awaiting your answer. The response includes waiting_for_decision=true when the task is blocked on a decision. view="full" returns decision envelopes.
- Answer with orchestrator_submit_decision: fulfill|skip|abort|retry (admin also retry_with_skill:ID|create_skill). A fulfill payload must match the decision's context.kind: external_subagent takes a SubagentOutput {"status","confidence","result"}; plan takes the plan DAG {"steps":[...]}; dag_surgery offers no fulfill (skip|abort only). skip|abort|retry carry no payload. A delegation left unanswered keeps the step parked; that is a report, not a timeout -- answer it rather than cancelling.
- Async completion: notification.routes[] in config (trigger_on: ["task_completed","task_failed"], channel webhook|telegram, target_url/chat_id, optional secret_env HMAC, task_id_glob/step_id_glob filters) POSTs completion to your endpoint so no connection needs to stay open. Enable by setting system.notifications.enabled=true and configuring at least one route with enabled=true.
- Capability discovery: orchestrator_discover with no arguments lists capability domains; query="..." searches roles/SOPs by keyword; subsystem="config|schedule|exp|admin|jit|intel|proxy|..." returns that subsystem's action catalog including parameters. Tool schemas are authoritative in tools/list.`

// ServerInstructionsPublic is the variant surfaced on the public/Tier1 MCP
// server instance (VORTEX_PUBLIC_KEY). It intentionally omits any
// mention of orchestrator_run_command and orchestrator_invoke, since those
// tools are never registered on the public tier (see tools.RegisterTier1) --
// referencing a tool the caller cannot see is confusing at best and at worst
// invites a failed call to a nonexistent tool.
//
// ADDED (2026-07-06): this addresses a real, previously-undiscovered bug
// where public-tier callers had NO way to resolve a decision_required block
// -- orchestrator_submit_decision lived exclusively on Tier2 (see the
// tools.go change moving it to Tier1 with a tier-gated choice set). A public
// MCP client (e.g. HuggingChat) that submits a task, sees it get stuck in
// "blocked" status, and has no tool available to unstick it will eventually
// stop calling tools altogether and fabricate a "done" answer instead --
// this is believed to be a significant contributor to the low tool-
// utilization / premature-completion behavior observed with public clients.
const ServerInstructionsPublic = `orchestrator_mcp delegates multi-step, tool-using work to a supervised sub-agent. You are connected via the public tier: only orchestrator_submit_task, orchestrator_wait_task, orchestrator_get_task_status, orchestrator_discover, orchestrator_context_search, orchestrator_get_logs, and orchestrator_submit_decision are available.

Core Philosophy: "Professional Roles for Professional Tasks"
Use Orchestrator to prevent your system prompt from being cluttered with diverse domain knowledge. Delegating to specialized roles (software dev, research, etc.) keeps your context efficient and focused.

Decision Rules:
1. Prefer Orchestrator (orchestrator_submit_task) when the task requires specialized expertise, planning, coordination across multiple tools, or intermediate judgment.
2. Prefer direct tool calls for simple, independent operations.
3. Do NOT submit single-step tasks here.

Capability Awareness:
The orchestrator relies on the configured roles and MCP servers in the current environment (see orchestrator_submit_task for the active list). If a required capability is missing, adapt your execution plan instead of assuming unsupported functionality.

Task lifecycle (stable contract):
- Submit with orchestrator_submit_task. Role binding is per step: pass steps:[{"id","role_id","task"}]. A step without an explicit role_id cannot run; there is no default-role fallback.
- Transport semantics: stdio clients MUST close the loop in a single connection (submit -> wait -> answer decisions -> result). The stdio MCP protocol has no server-push primitive, so a disconnected client cannot be woken up. HTTP/SSE clients can alternatively stream events via /api/events. For work that outlives your connection, configure a notification route (see Async completion below).
- orchestrator_wait_task is event-driven: it returns on completion or when a new decision appears, and a timeout return is NOT a failure. Check pending_decisions after every wait return -- status may read "running" while the task is parked awaiting your answer. The response includes waiting_for_decision=true when the task is blocked on a decision.
- Answer with orchestrator_submit_decision: choice "fulfill", "skip", "abort", or "retry". A fulfill payload must match the decision's context.kind: external_subagent takes a SubagentOutput {"status","confidence","result"}; plan takes the plan DAG {"steps":[...]}; dag_surgery offers no fulfill (skip|abort only).
- Async completion: notification.routes[] in config (trigger_on: ["task_completed","task_failed"], channel webhook|telegram, target_url/chat_id, optional secret_env HMAC, glob filters) POSTs completion to your endpoint so no connection needs to stay open. Enable by setting system.notifications.enabled=true and configuring at least one route with enabled=true.
- Capability discovery: orchestrator_discover with no arguments lists capability domains; query="..." searches roles/SOPs by keyword; subsystem="..." returns that subsystem's action catalog including parameters. Tool schemas are authoritative in tools/list.`

// ResolveServerInstructions returns the text that should be passed to
// server.WithInstructions(...) for either the admin/Tier2 server (public =
// false) or the public/Tier1 server (public = true). If the corresponding
// override in reg.PromptOverrides is non-empty (i.e. someone has dropped a
// file into workspace/prompts/server_instructions.txt or
// server_instructions_public.txt), that text is used verbatim in place of
// the hardcoded ServerInstructions/ServerInstructionsPublic constants above
// -- letting the server's initialize-time instructions be edited and
// hot-reloaded (via the existing StartWatcher poll) without a recompile,
// the same way role/skill prompts already can be. A nil reg or empty
// override falls back to the compiled-in constant, so this is safe to call
// unconditionally. ADDED (2026-07-28).
func ResolveServerInstructions(reg *config.Registry, public bool) string {
	if reg != nil {
		if public && reg.PromptOverrides.ServerInstructionsPublic != "" {
			return reg.PromptOverrides.ServerInstructionsPublic
		}
		if !public && reg.PromptOverrides.ServerInstructions != "" {
			return reg.PromptOverrides.ServerInstructions
		}
	}
	if public {
		return ServerInstructionsPublic
	}
	return ServerInstructions
}
