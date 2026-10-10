package core

// prompt_constants.go centralizes hardcoded prompt injection strings used
// across the core package. This is the single source of truth for:
//   - Bracket-style injection markers ([RECOVERY CONTEXT], [SYSTEM], etc.)
//   - System prompt templates (intent classifier, voting aggregator, etc.)
//   - Tool-failure message templates (eliminates spawner_utils/chat_harness dupes)
//   - Healer advice strings
//   - Chat harness system prompt sections
//
// config/prompt_overrides.go can override some of these at runtime via config.
// config/seed_builtins.go holds built-in role instructions (separate concern).

// ── Injection Markers (bracket-style) ─────────────────────────────────

const (
	// MarkerRecoveryContext wraps AdditionalPromptContext in user text.
	MarkerRecoveryContext = "[RECOVERY CONTEXT]"
	// MarkerRefinementRequired wraps failed-verification retry prompts.
	MarkerRefinementRequired = "[REFINEMENT REQUIRED]"
	// MarkerSystemAdvice prefixes repair-skill suggestions.
	MarkerSystemAdvice = "[ADVICE]"
	// MarkerSystemError prefixes invalid-tool-format corrections.
	MarkerSystemError = "[SYSTEM ERROR]"
	// MarkerSystem prefixes general system-injected messages.
	MarkerSystem = "[SYSTEM]"
	// MarkerSystemSelfHealing prefixes healer advice.
	MarkerSystemSelfHealing = "[SYSTEM SELF-HEALING ADVICE]"
	// MarkerEnvObservation prefixes environment delta feedback.
	MarkerEnvObservation = "[ENVIRONMENT OBSERVATION]"
	// MarkerSystemFeedback prefixes cycle-breaker feedback.
	MarkerSystemFeedback = "[SYSTEM FEEDBACK]"
	// MarkerSystemDegrade prefixes cycle-breaker degrade notice.
	MarkerSystemDegrade = "[SYSTEM DEGRADE]"
	// MarkerPrevFailureAntiPattern wraps historical pitfall warnings.
	MarkerPrevFailureAntiPattern = "[PREVIOUS FAILURE ANTI-PATTERN]"
	// Marker8dLearnedExperiencePrecedent wraps experience-graph injection.
	MarkerLearnedExperience = "[LEARNED EXPERIENCE PRECEDENT]"
	// MarkerHistoricalPitfall wraps anti-pattern store injection.
	MarkerHistoricalPitfall = "[HISTORICAL PITFALL WARNING]"
	// MarkerRepetitiveOutput is the steer message for stream repetition.
	MarkerRepetitiveOutput = "[REPETITIVE_OUTPUT detected. You are producing repetitive output. Stop and provide a direct, concise answer to the user.]"
	// MarkerNoTextResponse is the steer message for tool-only turns.
	MarkerNoTextResponse = "[You have been making tool calls without producing a response for 3 turns. Provide a direct answer to the user now.]"
	// MarkerCompressedPlaceholder replaces compressed instruction body.
	MarkerCompressedPlaceholder = "[... instruction body compressed for brevity ...]"
	// MarkerHistoryCompressed replaces compressed conversation history.
	MarkerHistoryCompressed = "[... earlier conversation history compressed to fit within context budget ...]"
)

// ── System Prompt Templates ───────────────────────────────────────────

const (
	// SysPromptIntentClassifier is the system prompt for capability classification.
	SysPromptIntentClassifier = "You are an intent classifier for an autonomous agent orchestrator."

	// SysPromptChatSummary is the system prompt for conversation summarization.
	SysPromptChatSummary = "Summarize the following conversation concisely, preserving key decisions, file paths, and constraints."

	// SysPromptBestAnswer is the fallback system prompt when forcing a final answer.
	SysPromptBestAnswer = "Provide your best answer based on the information available in the conversation."

	// SysPromptOrchestrationDispatcher is the fallback role instruction when no role matches.
	SysPromptOrchestrationDispatcher = "You are the orchestration dispatcher. A specific role could not be generated for this task. Analyze the task, select the most relevant available tools and skills, and execute directly. Delegate sub-tasks to available roles when applicable. Prioritize tool-based execution over raw reasoning."

	// SysPromptSpecializedTool is the system prompt for JIT tool promotion.
	// Format: SysPromptSpecializedTool with %s=toolID, %s=description.
	SysPromptSpecializedTool = "You have access to a specialized tool `%s` that was generated to solve: %s. Use it when appropriate."
)

// ── Chat Harness System Prompt Sections ───────────────────────────────

const (
	// ChatHarnessRolePrefix is the role header format for chat harness.
	ChatHarnessRolePrefix = "# Role: %s\n%s\n\n"

	// ChatHarnessDefaultIdentity is the default system prompt when no role is set.
	ChatHarnessDefaultIdentity = "You are a concise, helpful assistant in a standalone chat harness.\n"

	// ChatHarnessToolsetDesc describes the available tools.
	ChatHarnessToolsetDesc = "You have a small toolset: write_file, read_file, execute_code, delegate_to_orchestrator.\n"

	// ChatHarnessUsageGuidance instructs on tool usage.
	ChatHarnessUsageGuidance = "Use tools when they help; answer directly otherwise. Do not call tools you do not need.\n"

	// ChatHarnessDelegatePrefix introduces the delegation readiness gate.
	ChatHarnessDelegatePrefix = "Before calling delegate_to_orchestrator, you MUST verify the task is fully specified:\n"

	// ChatHarnessDelegateRule1 checks for concrete parameters.
	ChatHarnessDelegateRule1 = "1. All required parameters (paths, identifiers, formats) are concrete — no <TBD>/placeholder.\n"

	// ChatHarnessDelegateRule2 checks for algorithmic ambiguity.
	ChatHarnessDelegateRule2 = "2. No algorithmic ambiguity (e.g. 'sort it' without specifying key/order).\n"

	// ChatHarnessDelegateRule3 checks for path contradictions.
	ChatHarnessDelegateRule3 = "3. No path or identifier contradictions across the request.\n"
)

// ── Tool-Failure Message Templates ────────────────────────────────────
// These eliminate the duplication between spawner_utils.go and chat_harness.go.

const (
	// ToolFailRepeatedFmt is the message when a tool has failed N times.
	// %s=toolName, %d=failCount. Used by both spawner_utils and chat_harness.
	ToolFailRepeatedFmt = "[TOOL %s HAS FAILED %d TIMES] Stop using this tool. Use an alternative approach or answer directly without tools."

	// ToolFailOnceFmt is the message for a single tool failure.
	// %s=toolName, %s=errorMsg. Used by both spawner_utils and chat_harness.
	ToolFailOnceFmt = "[TOOL FAILED: %s] %s\nDo not abort. Either fix the arguments and retry, use a different tool, or answer directly."

	// ToolFailSERFFmt is the SERF-classified tool failure message.
	// %s=toolName, %s=category, %s=recoveryAdvice.
	ToolFailSERFFmt = "[TOOL FAILED: %s] Category: %s. %s"
)

// ── Healer Advice Strings ─────────────────────────────────────────────

const (
	HealerAdviceElementNotInteractable = "The UI element you tried to interact with is hidden or not ready. Try calling 'wait_for_selector' or taking a 'screenshot' to re-verify the page state before retrying."
	HealerAdviceTimeout               = "The operation timed out. This could be due to slow network or heavy page load. Consider increasing the timeout parameter or breaking the task into smaller sub-steps."
	HealerAdviceSelectorNotFound     = "The CSS selector you provided was not found on the page. Use 'get_page_source' or 'screenshot' to check if the page structure has changed or if you are on the wrong URL."
	HealerAdvicePermissionDenied      = "You hit a permission barrier. Check if you are correctly logged in or if you need to request elevated access for this specific tool."
	HealerAdviceRateLimit             = "Rate limit reached. Please pause for a few seconds before retrying, or reduce the frequency of your requests."
	HealerAdviceContextWindow         = "Context overflow risk. Try summarizing the previous steps or removing redundant logs from your next action."
)

// ── Spawner System Injection Messages ────────────────────────────────

const (
	// SpawnerNoToolsMsg is injected when the model hasn't called any tools.
	SpawnerNoToolsMsg = "[SYSTEM]: You have not executed any tools yet. Please proceed to actually call the necessary tool(s) now."

	// SpawnerNoToolsBadJSONMsg is injected when no tools + invalid JSON output.
	SpawnerNoToolsBadJSONMsg = `[SYSTEM]: You have not executed any tools yet, AND your previous output was not valid JSON. You MUST: (1) call the necessary tool(s) using the native JSON tool call structure, (2) ensure your final output is valid JSON with "status", "confidence", and "result" fields. Do not repeat the same formatting mistake.`

	// SpawnerInvalidToolFormatMsg is injected when the model uses XML pseudo-tags.
	SpawnerInvalidToolFormatMsg = "[SYSTEM ERROR]: You attempted to invoke a tool using invalid XML/pseudo-tags (e.g. <anim:call>). You MUST use the native JSON tool call structure provided by the platform. Please immediately issue the correct tool call in JSON format."

	// SpawnerLoopDetectedFmt is injected when a tool-call loop is detected.
	// %s=reason.
	SpawnerLoopDetectedFmt = "[SYSTEM]: Loop detected (%s). You are repeating the same tool calls. Change your approach — try different parameters, a different tool, or provide a final answer with current information."

	// SpawnerLoopPersistMsg is the second-warning for persistent tool loops.
	SpawnerLoopPersistMsg = "[SYSTEM]: You are still repeating tool calls. Stop calling tools and provide your best answer with the information you already have."

	// SpawnerVDAPrompt is the VDA verification instruction.
	SpawnerVDAPrompt = "\n\n### VDA Verification Required\nState-changing tools were executed. A verification screenshot has been attached. Please analyze the image to verify if the UI state matches your expectation before proceeding."
)
