package core

import (
	"fmt"
	"os"
	"strings"

	"github.com/daybeam/vortex/config"
)

// BuildSystemManual returns a tier-filtered system manual for the frontend model.
//
// The frontend model (chat harness LLM) directly interacts with users and needs
// to understand the system it runs in: what tools are available, how to use them
// safely, what configuration is in effect, and what operations are dangerous.
//
// Topics:
//   - "" or "overview" — system identity + capabilities summary
//   - "tools"          — tool catalog with safety classification
//   - "config"         — current configuration (tier-filtered)
//   - "safety"         — dangerous operations + admin-only warnings
//
// Tier filtering:
//   - "guest"      — overview + safe tools only (no config, no admin ops)
//   - "authorized" — + non-sensitive config + tool details
//   - "admin"      — everything (full config, provider details, limits)
//
// Sensitive data (API keys, tokens, passwords) is NEVER included regardless of tier.
func BuildSystemManual(reg *config.Registry, topic, tier string) string {
	if reg == nil {
		return "System manual unavailable: no registry."
	}
	t := normalizeTier(tier)
	topic = strings.ToLower(strings.TrimSpace(topic))
	if topic == "" {
		topic = "overview"
	}

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	switch topic {
	case "overview":
		return manualOverview(reg, t)
	case "tools":
		return manualTools(reg, t)
	case "config":
		return manualConfig(reg, t)
	case "safety":
		return manualSafety(reg, t)
	default:
		return fmt.Sprintf("Unknown topic %q. Available: overview, tools, config, safety.", topic)
	}
}

func normalizeTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "admin":
		return "admin"
	case "authorized":
		return "authorized"
	default:
		return "guest"
	}
}

// ── Overview ─────────────────────────────────────────────────────────

func manualOverview(reg *config.Registry, tier string) string {
	var b strings.Builder
	b.WriteString("# Orchestrator System Overview\n\n")
	b.WriteString("You are the frontend assistant in an AI orchestrator system. ")
	b.WriteString("You chat directly with users, answer questions, and delegate complex tasks.\n\n")

	b.WriteString("## Architecture\n")
	b.WriteString("- **Hub mode**: You are the user-facing gateway. You decide when to handle directly vs. delegate.\n")
	b.WriteString("- **Delegation**: `delegate_to_orchestrator` runs a full role-based task with planning + subagents.\n")
	b.WriteString("- **Roles**: Specialized AI roles (researcher, coder, analyst, etc.) execute delegated work.\n")
	b.WriteString("- **Skills**: Reusable prompt fragments that roles can invoke.\n")
	b.WriteString("- **MCPs**: External tool servers (filesystem, browser, databases, etc.).\n\n")

	b.WriteString("## Capabilities\n")
	fmt.Fprintf(&b, "- %d role(s) available\n", len(reg.Roles))
	fmt.Fprintf(&b, "- %d skill(s) available\n", len(reg.Skills))
	fmt.Fprintf(&b, "- %d MCP(s) configured\n", len(reg.MCPs))
	fmt.Fprintf(&b, "- %d provider(s) configured\n", len(reg.Providers))
	if reg.DefaultProvider != "" {
		fmt.Fprintf(&b, "- Default provider: %s\n", reg.DefaultProvider)
	}
	if len(reg.GlobalFallback) > 0 {
		b.WriteString("- Global fallback: ENABLED (providers without their own fallback chain use it)\n")
	}
	b.WriteString("\n")

	b.WriteString("## Your Tools\n")
	b.WriteString("- `read_file` — read file contents (safe)\n")
	b.WriteString("- `write_file` — write file contents (safe within workspace)\n")
	b.WriteString("- `execute_code` — run Python/Node/Lua code in sandbox (moderate risk)\n")
	b.WriteString("- `delegate_to_orchestrator` — delegate complex tasks (safe)\n")
	b.WriteString("- `orchestrator_task_status` — check delegated task status (safe)\n")
	b.WriteString("- `orchestrator_help` — query this system manual (safe)\n\n")

	b.WriteString("## Quick Guide\n")
	b.WriteString("- **Simple questions**: Answer directly, no tools needed.\n")
	b.WriteString("- **File operations**: Use read_file/write_file. Paths are relative to workspace.\n")
	b.WriteString("- **Complex tasks**: Use delegate_to_orchestrator (multi-step, multi-file, specialized roles).\n")
	b.WriteString("- **Check status**: Use orchestrator_task_status with the task id.\n")
	b.WriteString("- **Learn more**: Call orchestrator_help with topic=\"tools\", \"config\", or \"safety\".\n\n")

	if tier == "guest" {
		b.WriteString("> You are a **guest** user. Some features may be restricted. Ask the admin for elevated access if needed.\n")
	}

	return b.String()
}

// ── Tools ────────────────────────────────────────────────────────────

func manualTools(reg *config.Registry, tier string) string {
	var b strings.Builder
	b.WriteString("# Tool Catalog\n\n")

	b.WriteString("## Core Tools (always available)\n\n")
	b.WriteString("| Tool | Safety | Description |\n|---|---|---|\n")
	b.WriteString("| `read_file` | ✅ Safe | Read file contents. Path relative to workspace. |\n")
	b.WriteString("| `write_file` | ✅ Safe | Write/create files. Restricted to workspace root. |\n")
	b.WriteString("| `execute_code` | ⚠️ Moderate | Run Python/Node/Lua in sandbox. Cannot access host shell. |\n")
	b.WriteString("| `delegate_to_orchestrator` | ✅ Safe | Delegate to role-based orchestrator. Returns task id. |\n")
	b.WriteString("| `orchestrator_task_status` | ✅ Safe | Check status of a delegated task. |\n")
	b.WriteString("| `orchestrator_help` | ✅ Safe | Query this system manual. |\n\n")

	b.WriteString("## Tool Usage Guide\n\n")
	b.WriteString("### read_file\n")
	b.WriteString("- Parameters: `path` (string, relative to workspace), `start_line`/`end_line` (optional, for partial reads)\n")
	b.WriteString("- Safe for all users. Reads only within the workspace.\n\n")

	b.WriteString("### write_file\n")
	b.WriteString("- Parameters: `path` (string), `content` (string)\n")
	b.WriteString("- Safe within workspace. Cannot write outside workspace root.\n\n")

	b.WriteString("### execute_code\n")
	b.WriteString("- Parameters: `language` (python/node/lua), `code` (source code string)\n")
	b.WriteString("- **IMPORTANT**: `code` is SOURCE CODE, not shell commands. `git fetch` will fail.\n")
	b.WriteString("- Runs in a sandbox with memory/CPU limits.\n")
	if tier != "guest" {
		fmt.Fprintf(&b, "- Sandbox limits: %dMB memory, %d CPU seconds\n", reg.System.SandboxedMemoryMB, reg.System.SandboxedCPUSecs)
	}
	b.WriteString("\n")

	b.WriteString("### delegate_to_orchestrator\n")
	b.WriteString("- Parameters: `prompt` (string — the task description)\n")
	b.WriteString("- Returns: task id + initial status. Work runs in background.\n")
	b.WriteString("- Use for: multi-file tasks, specialized roles, long-running work.\n")
	b.WriteString("- **Readiness gate**: Verify the task is fully specified before delegating.\n\n")

	if tier != "guest" {
		b.WriteString("## Available Roles\n")
		for name, role := range reg.Roles {
			desc := role.Instruction
			if len(desc) > 80 {
				desc = desc[:77] + "..."
			}
			fmt.Fprintf(&b, "- **%s**: %s\n", name, desc)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// ── Config ───────────────────────────────────────────────────────────

func manualConfig(reg *config.Registry, tier string) string {
	if tier == "guest" {
		return "# Configuration\n\nConfiguration details are not available for guest users. Ask the admin for specifics.\n"
	}

	var b strings.Builder
	b.WriteString("# Current Configuration\n\n")

	// Providers
	b.WriteString("## Providers\n")
	if reg.DefaultProvider != "" {
		fmt.Fprintf(&b, "- Default: **%s**\n", reg.DefaultProvider)
	}
	for name, pc := range reg.Providers {
		model := pc.Model
		if model == "" {
			model = "(default)"
		}
		fbStatus := "none"
		if len(pc.FallbackChain) > 0 {
			fbStatus = fmt.Sprintf("%d provider(s)", len(pc.FallbackChain))
		}
		fmt.Fprintf(&b, "- **%s**: model=%s, fallback=%s", name, model, fbStatus)
		if tier == "admin" {
			if pc.BaseURL != "" {
				fmt.Fprintf(&b, ", base_url=%s", pc.BaseURL)
			}
			if pc.APIKeyEnv != "" {
				fmt.Fprintf(&b, ", api_key_env=%s", pc.APIKeyEnv)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Global fallback
	b.WriteString("## Global Fallback\n")
	if len(reg.GlobalFallback) > 0 {
		b.WriteString("- Status: **ENABLED**\n")
		for i, fb := range reg.GlobalFallback {
			fmt.Fprintf(&b, "  %d. provider=%s, model=%s\n", i+1, fb.Provider, fb.Model)
		}
		b.WriteString("- Applies to: any provider without its own fallback_chain\n")
	} else {
		b.WriteString("- Status: disabled (not configured)\n")
	}
	b.WriteString("\n")

	// System settings (admin only for limits)
	if tier == "admin" {
		b.WriteString("## System Settings\n")
		fmt.Fprintf(&b, "- Max tool turns: %d\n", reg.System.MaxToolTurns)
		fmt.Fprintf(&b, "- Max spawn depth: %d\n", reg.System.MaxSpawnDepth)
		fmt.Fprintf(&b, "- Sandbox memory: %dMB\n", reg.System.SandboxedMemoryMB)
		fmt.Fprintf(&b, "- Sandbox CPU: %d seconds\n", reg.System.SandboxedCPUSecs)
		fmt.Fprintf(&b, "- Delegation mode: %v\n", reg.System.DelegationMode)
		fmt.Fprintf(&b, "- Non-interactive: %v\n", reg.System.NonInteractive)
		fmt.Fprintf(&b, "- Confidence threshold: %.2f\n", reg.System.ConfidenceThreshold)
		b.WriteString("\n")
	}

	// Channels
	b.WriteString("## Channels\n")
	if os.Getenv("TELEGRAM_BOT_TOKEN") != "" {
		b.WriteString("- Telegram: configured\n")
	} else {
		b.WriteString("- Telegram: not configured\n")
	}
	b.WriteString("\n")

	return b.String()
}

// ── Safety ───────────────────────────────────────────────────────────

func manualSafety(reg *config.Registry, tier string) string {
	var b strings.Builder
	b.WriteString("# Safety Guide\n\n")

	b.WriteString("## Safety Classification\n\n")
	b.WriteString("| Level | Description | Tools |\n|---|---|---|\n")
	b.WriteString("| ✅ Safe | No risk to host or data | read_file, delegate_to_orchestrator, orchestrator_task_status, orchestrator_help |\n")
	b.WriteString("| ⚠️ Moderate | Bounded risk, sandboxed | execute_code, write_file |\n")
	b.WriteString("| 🔴 Admin-only | Can affect host system | (none in chat harness — admin ops are via web UI) |\n\n")

	b.WriteString("## Rules for All Users\n")
	b.WriteString("1. **Workspace isolation**: read_file/write_file are restricted to the workspace root. Paths cannot escape via `../`.\n")
	b.WriteString("2. **Sandboxed execution**: execute_code runs in a sandbox with memory/CPU limits. It cannot access the host shell.\n")
	b.WriteString("3. **No secrets exposure**: API keys, tokens, and passwords are never returned in any tool output or help text.\n")
	b.WriteString("4. **Delegation is safe**: delegate_to_orchestrator runs in a separate context. It cannot modify the host.\n\n")

	if tier == "admin" {
		b.WriteString("## Admin-Only Information\n")
		b.WriteString("- Admin operations (config changes, role/skill management) are performed via the web UI, not chat tools.\n")
		b.WriteString("- The chat harness deliberately does NOT expose config-modification tools to prevent accidental damage.\n")
		b.WriteString("- Telegram admin users can use slash commands: /cancel, /config, /mcp, /run (admin tier only).\n\n")
	} else {
		b.WriteString("## Guest/Authorized Limitations\n")
		b.WriteString("- Configuration changes require admin access (web UI or Telegram admin commands).\n")
		b.WriteString("- Admin slash commands (/cancel, /config, /mcp, /run) are restricted to admin tier.\n")
		b.WriteString("- You can still: read files, write files, execute code, delegate tasks, and check status.\n\n")
	}

	b.WriteString("## Host Damage Prevention\n")
	b.WriteString("- `execute_code` is sandboxed: no network access (unless explicitly configured), no host filesystem access beyond workspace.\n")
	b.WriteString("- `write_file` cannot write outside the workspace root.\n")
	b.WriteString("- There is no `delete_file` or `run_command` tool in the chat harness — deletion and shell commands are not exposed.\n")
	b.WriteString("- The orchestrator backend (accessed via delegation) has its own capability enforcement and sandbox.\n")

	return b.String()
}
