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
const ServerInstructions = `Vortex delegates multi-step, tool-using work to a supervised sub-agent.

Core Philosophy: "Professional Roles for Professional Tasks"
Use Vortex to prevent your system prompt from being cluttered with diverse domain knowledge (e.g., software dev, market research, content writing, platform-specific automation). Delegating to specialized roles ensures context efficiency and higher quality by isolating domain-specific instructions from your main context.

Decision Rules:
1. Before manually chaining multiple tool calls, consider whether the entire task should be delegated to Vortex.
2. Prefer Vortex (orchestrator_submit_task) when the task requires:
   - Specialized domain expertise (delegating to a professional role).
   - Planning and logical sequencing before execution.
   - Coordination across multiple tools or MCP servers.
   - Intermediate judgment between steps.
   - Automatic retries or recovery from failures.
   - Long-running execution (delegation).
3. Prefer direct tool calls ONLY for simple, independent operations (e.g., one search, one filesystem read, one single-step shell command).
4. Do NOT use Vortex for single-step tasks that can be completed directly.

Capability Awareness:
Vortex itself does not provide domain-specific capabilities. It relies on the configured roles and MCP servers in the current environment (see orchestrator_submit_task description for the active list). If a required capability is missing, adapt your execution plan instead of assuming unsupported functionality.

After submitting, use orchestrator_wait_task / orchestrator_get_task_status to check progress. If status becomes "blocked" with pending_decisions, call orchestrator_submit_decision (skip|abort|retry, or admin-only retry_with_skill:ID|create_skill) to unblock it.`

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
const ServerInstructionsPublic = `Vortex delegates multi-step, tool-using work to a supervised sub-agent. You are connected via the public tier: only orchestrator_submit_task, orchestrator_wait_task, orchestrator_get_task_status, orchestrator_discover, orchestrator_analyze_task, orchestrator_get_logs, and orchestrator_submit_decision are available.

Core Philosophy: "Professional Roles for Professional Tasks"
Use Vortex to prevent your system prompt from being cluttered with diverse domain knowledge. Delegating to specialized roles (software dev, research, etc.) keeps your context efficient and focused.

Decision Rules:
1. Prefer Vortex (orchestrator_submit_task) when the task requires specialized expertise, planning, coordination across multiple tools, or intermediate judgment.
2. Prefer direct tool calls for simple, independent operations.
3. Do NOT submit single-step tasks here.

Capability Awareness:
The orchestrator relies on the configured roles and MCP servers in the current environment (see orchestrator_submit_task for the active list). If a required capability is missing, adapt your execution plan instead of assuming unsupported functionality.

After submitting, use orchestrator_wait_task / orchestrator_get_task_status to check progress. If status becomes "blocked", call orchestrator_submit_decision with choice "skip", "abort", or "retry" to move forward.`

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
