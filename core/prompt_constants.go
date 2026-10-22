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
	// MarkerPreviousOutput wraps the delta-backtrack previous output summary.
	// ADDED (2026-10-17). See docs/plan/DELTA_BACKTRACK_DESIGN.md.
	MarkerPreviousOutput = "[PREVIOUS OUTPUT]"
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
	HealerAdviceTimeout                = "The operation timed out. This could be due to slow network or heavy page load. Consider increasing the timeout parameter or breaking the task into smaller sub-steps."
	HealerAdviceSelectorNotFound       = "The CSS selector you provided was not found on the page. Use 'get_page_source' or 'screenshot' to check if the page structure has changed or if you are on the wrong URL."
	HealerAdvicePermissionDenied       = "You hit a permission barrier. Check if you are correctly logged in or if you need to request elevated access for this specific tool."
	HealerAdviceRateLimit              = "Rate limit reached. Please pause for a few seconds before retrying, or reduce the frequency of your requests."
	HealerAdviceContextWindow          = "Context overflow risk. Try summarizing the previous steps or removing redundant logs from your next action."
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

	// SpawnerZeroEffectMsg is injected when the model declares status=ok but the
	// trace contains no side-effecting tool call (read-only calls only).
	//
	// Why this exists (eval v14, 2026-10-21): AppWorld task d4e9306_1 ("Follow
	// artists of all the songs and albums I have ever liked on Spotify") was run
	// five times on the same config. In 4/5 runs the model called exactly two
	// read-only tools (supervisor__show_profile, supervisor__show_account_passwords)
	// — trace=2 — then emitted {"status":"ok","confidence":0.9,"result":{"reply":
	// "Logged into Spotify and retrieved liked songs and albums. Now following all
	// artists..."}}. The engine accepted it because the only completion gate was
	// `noToolCallsYet := len(trace) == 0`, which a read-only trace already defeats.
	// The task scored 1/6 every time. Liveness signal, not a semantic judgement:
	// we only assert "you claim ok, yet nothing you did could have changed state".
	SpawnerZeroEffectMsg = "[SYSTEM]: You declared status=ok, but none of the tool calls in this step could have changed any external state — every call so far was read-only. " +
		"If the task requires changing data (adding, updating, deleting, sending, following, downloading, paying), you must actually execute those calls now; describing the result is not the same as producing it. " +
		"If this really is a read-only question (e.g. 'how many...', 'which one...'), put the concrete answer in `result` as a bare value and state explicitly that it came from a read-only query."

	// SpawnerZeroEffectParseFailMsg is the parse-failure variant of the
	// zero-effect nudge (A2, eval FINDINGS.md §A2, 2026-10-22). The gate
	// originally required StatusOK, so a model whose output ParseOutput
	// degraded to partial (conf 0.3 fallback) was never nudged even when its
	// trace was entirely read-only — the v20 collapse: correct plans written
	// inside unparseable text, zero corrective signal from any gate. `partial`
	// here is the engine's own degradation artifact, not the model declaring
	// completion, so parseFailed turns are treated like completion claims.
	// The wording names BOTH problems (invalid format AND read-only trace):
	// reusing the ok-variant would falsely assert "you declared status=ok"
	// about an output the engine could not read.
	SpawnerZeroEffectParseFailMsg = "[SYSTEM]: Your last output could not be parsed as the required JSON structure, so the step is not complete. In addition, every tool call so far has been read-only — nothing has changed external state yet. " +
		"If the task requires changing data (adding, updating, deleting, sending, following, downloading, paying), issue the actual tool call now using the native JSON tool-call structure — do not describe it in prose or pseudo-tags. " +
		"If this really is a read-only question, re-send your final answer as valid JSON with the concrete value in `result`."

	// SpawnerMidAbandonMsg is injected by the mid-abandonment gate (A5, eval
	// FINDINGS.md §A5, 2026-10-22): the model started working (read-only calls
	// in the trace), then emitted a PARSEABLE partial and stopped while turns
	// remain. This is the small-model failure mode all three completion gates
	// were blind to: noToolCallsYet is already false (work started), the
	// zero-effect gate wants ok/parseFailed (this is a clean partial), and the
	// parseable partial is accepted as-is right after the gates.
	//
	// The wording leaves a legitimate exit open on purpose: a model that is
	// genuinely blocked (missing information) is told to finalize and SAY what
	// is missing instead of being forced into a loop — the nudge is one-shot.
	SpawnerMidAbandonMsg = "[SYSTEM]: You reported this step as incomplete while turns remain, and nothing you have called so far could have changed external state — every call was read-only. " +
		"If the task requires changing data (adding, updating, deleting, sending, following, downloading, paying), continue now and issue those calls. " +
		"If you genuinely cannot proceed, finalize now and state exactly what is missing in `missing_context` — do not simply stop."
)
