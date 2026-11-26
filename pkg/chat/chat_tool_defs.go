package chat

import (
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

func (h *ChatHarness) ToolDefinitions() []schemas.ToolDefinition {
	src := core.CoreToolDefinitions()
	tools := make([]schemas.ToolDefinition, len(src))
	copy(tools, src)
	filtered := tools[:0]
	for _, t := range tools {
		if t.Name != "decompress" {
			filtered = append(filtered, t)
		}
	}
	tools = filtered
	if h.SessionRoot != "" {
		for i := range tools {
			switch tools[i].Name {
			case "write_file":
				tools[i].Description = "Write content to a file. Path is relative to the workspace root (" +
					h.SessionRoot + "). Can write anywhere under the workspace."
			case "read_file":
				tools[i].Description = "Read content from a file. Path is relative to the workspace root (" +
					h.SessionRoot + ")."
			case "execute_code":
				tools[i].Description = "Execute code in a sandboxed environment. Supports: python, node, bun, lua. " +
					"Returns stdout, stderr, and exit code.\n\n" +
					"IMPORTANT: The 'code' parameter is SOURCE CODE, not shell commands. " +
					"Do NOT pass 'git fetch', 'ls -F', or other shell commands — they will fail with SyntaxError. " +
					"Use python/node code only.\n\n" +
				"Good use cases:\n" +
				"- Counting functions/lines/patterns across many files\n" +
				"- Extracting structured data from logs/JSON/CSV\n" +
				"- Comparing or aggregating file contents"
			}
		}
	}
	tools = append(tools, schemas.ToolDefinition{
		Name: "delegate_to_orchestrator",
		Description: "Delegate a complex, multi-step task to the orchestrator (role-based subagent execution). " +
			"USE THIS when the request spans several files/sources/sub-questions, needs several independent " +
			"tool rounds, needs a specialized role, or is long enough that the user would rather get a task id " +
			"now and follow up. " +
			"Returns immediately with a task id and the task's current status; the work continues in the " +
			"background, and this session is resumed automatically when it finishes so the result can be " +
			"delivered then. Use orchestrator_task_status only when the answer cannot wait for that. " +
			"READINESS GATE (mandatory): Before calling this tool, verify the task is fully specified — " +
			"all required parameters present, no algorithmic ambiguity, no path/identifier contradictions, " +
			"and the expected outcome is concrete. If anything is unclear, try to fill in the minimum " +
			"missing information yourself first; only if you genuinely cannot, ask the user one focused " +
			"question. Delegating a vague prompt pollutes the downstream weak-model executor and wastes retry budget.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type": "string",
					"description": "Fully-specified task description to delegate. MUST include: (1) the concrete objective, " +
						"(2) any required inputs/paths/identifiers, (3) the expected output format or success criteria. " +
					"Do NOT delegate prompts containing placeholders like <TBD>, unspecified file paths, or unresolved " +
					"parameter choices — fill in the missing information yourself, or ask the user if you cannot.",
				},
				"readiness_self_check": map[string]any{
					"type": "boolean",
					"description": "Set to true to confirm you have verified: no missing parameters, no algorithmic ambiguity, " +
					"no path contradictions. Omit or set false only if you have filled in the missing information yourself " +
					"or asked the user and received clarification. This flag is the gateway's audit signal — delegating " +
					"without it true is a protocol violation.",
				},
			},
			"required": []string{"prompt"},
		},
	})
	tools = append(tools, schemas.ToolDefinition{
		Name: "orchestrator_task_status",
		Description: "Check a delegated task started by delegate_to_orchestrator. Returns the task's status, " +
			"its steps, the error if it failed, and the output files it produced. Use this to answer " +
			"\"how is it going\" and to collect the result once status is completed / failed / " +
			"completed_with_skips. Set wait_seconds to block briefly (e.g. 20) instead of polling in a loop.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": "The task id returned by delegate_to_orchestrator (form: task_xxxx).",
				},
				"wait_seconds": map[string]any{
					"type": "number",
					"description": "0 (default) = return the current status immediately. 1-60 = block up to " +
						"this many seconds for a terminal status. Keep it small: the chat turn is waiting.",
				},
			},
			"required": []string{"task_id"},
		},
	})
	tools = append(tools, schemas.ToolDefinition{
		Name: "orchestrator_help",
		Description: "Query the system manual to understand available tools, configuration, and safety rules. " +
			"Use this when you need to know what the system can do, how a tool works, or what configuration is in effect. " +
			"Returns tier-filtered information: guests see less than admins. No secrets (API keys/tokens) are ever exposed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic": map[string]any{
					"type": "string",
					"description": "Which section of the manual to read: \"overview\" (default), \"tools\", \"config\", or \"safety\". " +
						"overview = system identity + capabilities; tools = tool catalog + usage; config = current settings; safety = danger classification + host protection rules.",
					"enum": []string{"overview", "tools", "config", "safety"},
				},
			},
		},
	})
	if h.ScheduleExec != nil {
		tools = append(tools, scheduleToolDefs()...)
	}
	// hard arm of the post-delegation lockout (see post_deleg_lockout.go):
	// once a task has been delegated, the work tools leave the table entirely,
	// so the model cannot keep doing the task it just handed over. Applied
	// last on purpose — the orchestrator tools above must survive.
	//
	// Note the system prompt is NOT rebuilt to match: buildSystem's docstring
	// pins it as the cacheable prefix, so a per-turn string there would cost
	// more than the lockout saves. The tool table is the authority the model
	// calls against; core.ChatHarnessToolsetDesc is only prose.
	if h.pd.armMode() == PostDelegHard && h.pd.delegatedHere {
		kept := tools[:0]
		for _, t := range tools {
			if !postDelegWorkTools[t.Name] {
				kept = append(kept, t)
			}
		}
		tools = kept
	}
	return tools
}
