package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/core/stream"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ChatMessage is a single turn in a chat session. ID and ParentID form a
// tree: branching from a past message creates a new node with ParentID set
// to the replied-to message's ID. Empty ParentID means root.
type ChatMessage struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	// ClientID is the caller-supplied message id (web UI optimistic id,
	// Telegram message id). Stored so references sent by clients in later
	// turns (parent_id / reply_to) can be resolved to the stored ID —
	// without it, the active path breaks on reload because clients chain
	// onto ids the backend never persisted. See AppendUserMessage.
	ClientID string `json:"client_id,omitempty"`
	ReplyTo  string `json:"reply_to,omitempty"` // quoted message ID (Mode 2 — persisted so the UI can show "↩ Quoted" after refresh)
	// ForkedFrom references the branch point this message was forked from
	// (Mode 1, CHAT_BRANCHING_AND_QUOTE_DESIGN.md §3). Set when the message's
	// parent is NOT the previous active leaf (i.e. the conversation jumped
	// back to an earlier point). The UI renders a connector line from that
	// message down to this one.
	ForkedFrom string    `json:"forked_from,omitempty"`
	Role       string    `json:"role"` // "user" | "assistant" | "tool"
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// ChatEvent is a streamed event emitted during a chat run.
// Type is one of: delta | tool_call | tool_result | done | error.
type ChatEvent struct {
	Seq  int64          `json:"seq"`
	Type string         `json:"type"`
	Data string         `json:"data"`
	Meta map[string]any `json:"meta,omitempty"`
}

// ChatHarness executes a bounded agent loop over a single provider with a
// fixed tool set (core tools + optional delegate). It is intentionally NOT
// part of Spawner: it owns one tool contract and a small loop, avoiding the
// MCP schema-adapter and re-prompt churn that Spawner has accumulated.
// A ChatHarness is immutable after construction and safe for concurrent Run.
type ChatHarness struct {
	Provider   providers.Provider
	Model      string
	Role       *config.Role // when set, role's Instruction is prepended to system prompt
	Archive    *core.ContextArchive
	MemoryBank *store.MemoryBankStore
	Embed      core.EmbeddingClient
	OutputBase string
	// projectResolver resolves a taskID to its project space identifier for
	// ScopeProject cross-task recall (Phase 3). Nil-safe: ProjectOf returns
	// "default" when nil. Set via SetProjectResolver.
	projectResolver func(taskID string) string
	// Delegate hands off heavy multi-step work to the orchestrator.
	// Returns the orchestrator task id.
	Delegate func(prompt string) (string, error)
	// DelegateStatus reports the orchestrator's view of a task started by
	// Delegate. wait > 0 blocks up to that long for a terminal status (bounded
	// so a chat turn can never hang); wait == 0 reads the current state.
	// Optional: when nil, receipts carry only the task id and
	// orchestrator_task_status reports that it is unavailable.
	//
	// Why it exists: Delegate used to return nothing but the id, so the model
	// had no way to see the result it just paid for — execute_code returns its
	// output inline and therefore dominated it on every task. Measured: 0
	// delegations in 104 benchmark tasks.
	DelegateStatus func(taskID string, wait time.Duration) (map[string]any, bool)
	// HelpProvider returns a tier-filtered system manual for the orchestrator_help
	// tool. Set per-request by the API layer, closing over the user's tier so
	// execTool does not need to know about tiers. nil = feature unwired.
	HelpProvider func(topic string) string
	// OnTerminalTaskSeen is called when the model has just been shown a task's
	// terminal state — through orchestrator_task_status or the delegate receipt.
	// The API layer uses it to record that the *running turn* consumed that
	// result, so a follow-up wake queued for the same task can be suppressed
	// instead of answered twice. Without this signal the follow-up queue can
	// only choose between two defects: drop the wake (the result is lost and
	// nothing says so — harness-bench p24) or always deliver it (a duplicate
	// answer and a second turn for every task the live turn already handled —
	// the 026/v60 case the original claim rule was written for).
	//
	// Optional. Installed per request (it closes over the session id), so it is
	// deliberately not carried by cloneHarnessForRequest.
	OnTerminalTaskSeen func(taskID string)
	ScheduleExec       func(action string, args map[string]any) (string, error)
	MaxTurns           int
	// MaxOutputTokens caps the output tokens per LLM call. 0 = default 2048.
	// (ADDED 2026-10-28: previously hardcoded to 2048 with no config wiring
	// — harness-bench report §4.1.)
	MaxOutputTokens int
	// SessionRoot is the workspace root for path resolution in core tools.
	// When non-empty, a SafePathValidator is passed to core.HandleCoreTool so
	// relative paths resolve against the workspace instead of outputs/<taskID>.
	// When empty, behavior is unchanged (backward compatible).
	// (ADDED 2026-10-28: harness-bench report §4.2.)
	SessionRoot string
	// declaredArtifacts is the prompt's completion contract, parsed once at the
	// start of Run. nil = no contract. Per-request state; harness is per request.
	declaredArtifacts []string
	// contract: the same section split by column, plus the t=0 workspace
	// snapshot the extras check diffs against (RESULT §28.7). Zero = inert.
	contract contractScope
	// declaredFields is the other half of the same contract: for each declared
	// JSON artifact, the top-level keys the prompt says it must carry. Parsed
	// once at the start of Run. nil = the prompt states no fields, which must
	// leave behaviour exactly as it was before this check existed.
	//
	// Why it exists: 9 of 35 harness-bench 011 failures in v69–v71 had artifacts
	// that were present and (for 5 of them) arithmetically correct but shaped
	// wrongly — `candidates: [{type: "recommended"}]` instead of a top-level
	// `recommended`, or a participant map keyed by name instead of email.
	// Existence alone called every one of them done.
	declaredFields map[string][]string
	// taskPrompt is the last user message verbatim, captured once at the start
	// of Run. It is the only copy of the original specification this process
	// holds: the delegation path appends its two sections to whatever summary
	// the parent model wrote, because that summary drops binding clauses
	// (iron law 178 — "start/end" in 5/5 prompts, "Delete: none" in 4/5).
	// Empty outside a task-shaped prompt, which makes the append a no-op.
	taskPrompt string
	// sieve.Sieve is an optional repetition detector. If nil, a lightweight
	// signature-based fallback is used instead.
	Sieve *sieve.Sieve
	// Memory is an optional chat memory manager. If nil, full history
	// is passed to the provider unchanged (legacy behavior). When set,
	// history is windowed/summarized before the provider call.
	Memory ChatMemoryManager
	// Attachments are image/file attachments passed to the VLM provider
	// (e.g. split segments of a long screenshot). Set per-request via
	// shallow copy in ProcessMessage. Paths must be absolute.
	Attachments []schemas.Attachment

	// PendingSteer (ADDED 2026-10-10): non-blocking plan-drift correction
	// message. When non-empty, it is appended to the last tool result in
	// the current batch so the model sees it on the next turn.
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md §10.3.
	PendingSteer string
	SteerLock    sync.Mutex

	fileReadCalls     int // delegation drift: per-turn read-only tool call count
	delegationSteered bool

	// ReadinessGateSoft (ADDED 2026-10-10): when true, softens the
	// delegation readiness gate language from "NEVER blindly delegate"
	// to a suggestion. Weak models (≤7B) over-apply the hard rule and
	// ask excessive clarifying questions instead of acting. Default
	// false = strict (current behavior for strong models).
	// See eval-kit/docs/28-engine-path-analysis.md §2 claim ②.
	ReadinessGateSoft bool
	pd                postDelegState // post-delegation lockout, see post_deleg_lockout.go

	// MaxContextWindow is the model's context window size (in tokens),
	// from the provider config. Used for input size estimation: when
	// the user's input exceeds 50% of this, it is mounted as a file
	// with a summary; when it exceeds 100%, it is auto-delegated to
	// the orchestrator. 0 = no estimation (legacy behavior).
	MaxContextWindow int

	// Segments (ADDED 2026-10-10): optional topic segment manager for chat memory.
	// When non-nil, each indexed turn is processed by the core.SegmentManager
	// for topic segmentation (STAY/SEAL decisions). See CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
	Segments *core.SegmentManager
	SkillQuickPath func(query string) *QuickPathMatch // verified-skill quick path hook
}

// buildTurnContext returns the per-turn retrieval block, or "".
//
// It is appended to the tail of the last user message rather than the system
// prompt so that the prefix (system + history) stays byte-identical between
// turns and the provider can reuse its KV cache. Retrieval is keyed on the
// latest user message, so it can only ever be tail-stable, never prefix-stable.
// SetProjectResolver injects the taskID→projectID resolver.
// Nil-safe: when nil, ProjectOf returns "default".
func (h *ChatHarness) SetProjectResolver(r func(taskID string) string) {
	h.projectResolver = r
}

// ProjectOf returns the project space identifier for the given task.
// Returns "default" when no resolver is wired.
func (h *ChatHarness) ProjectOf(taskID string) string {
	if h.projectResolver == nil {
		return "default"
	}
	return h.projectResolver(taskID)
}

func (h *ChatHarness) buildTurnContext(query, taskID string) string {
	if h.Archive == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	forest, err := h.Archive.SearchForest(query, h.Embed, 5, 0.9, "", h.ProjectOf(taskID), taskID)
	if err != nil || len(forest.Nodes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Relevant prior context\n")
	for _, n := range forest.Nodes {
		if n.Summary != "" {
			b.WriteString("- " + n.Summary + "\n")
		}
	}
	return b.String()
}

// Run executes the agent loop and streams events via emit. It returns the
// final assistant text (for persistence) and any error.
func (h *ChatHarness) Run(ctx context.Context, taskID string, history []ChatMessage, emit func(ChatEvent) error) (output string, err error) {
	maxTurns := h.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	maxOutputTokens := h.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = 2048
	}

	query := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			query = history[i].Content
			break
		}
	}

	// ── Completion contract (stage 2c) ────────────────────────────────────
	// The declared artifact list is already inside the task prompt, so parsing
	// it needs no new parameter and no harness change. nil means "this prompt
	// carries no contract" — the ordinary case outside the benchmark — and
	// must leave behaviour exactly as it was.
	declaredArtifacts := h.initContract(query)
	h.declaredArtifacts = declaredArtifacts
	h.taskPrompt = query
	LogDelegationSpecOnce()
	// Same prompt, same parsing philosophy: the field lists sit in a
	// "Create `path` containing:" block, so no new parameter is needed.
	declaredFields := core.ParseDeclaredArtifactFields(query)
	h.declaredFields = declaredFields
	contractSteers := 0 // at most one per run; doubles as the latch and the metric
	// Bounded separately from contractSteers: a defect report must not consume
	// the one-shot done steer, or a run that fixes its plan after the report
	// would never be told it is finished. Two is enough for "here is what is
	// wrong" plus "here is what is still wrong"; beyond that it is nagging and
	// burns the turn budget.
	contractDefectSteers := 0
	const maxContractDefectSteers = 2
	// Resolved once per run so the arm a batch actually ran under is recorded
	// in the event, not inferred from the run-root name (→144). Default is
	// authorize, i.e. behaviour is unchanged when the env var is unset.
	completionContract := core.CompletionContractModeFromEnv()
	core.LogCompletionContractOnce()

	system := h.applyQuickPath(query, h.buildSystem(), emit)

	messages := make([]ChatMessage, 0, len(history)+maxTurns)
	if h.Memory != nil {
		history = h.Memory.Process(ctx, taskID, history)
	}
	messages = append(messages, history...)

	// Per-turn retrieval goes at the TAIL, never in the system prompt: the
	// prefix must stay byte-identical so the provider can reuse its cache.
	if ctxBlock := h.buildTurnContext(query, taskID); ctxBlock != "" {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				messages[i].Content += ctxBlock
				break
			}
		}
	}
	appendTimeContext(messages, history)
	// ── Tiered Auto-Handling for Oversized Inputs ───────────────────
	// When the user's input is large relative to the model's context window:
	//   Tier 1 (>50% of context): mount as file, replace with summary + reference
	//   Tier 3 (>100% of context): auto-delegate to orchestrator (DAG has compression)
	// Tier 2 (overflow recovery) is in the error handling below.
	// This is critical for small models (e.g., 4B with 4K context) receiving
	// large pastes from Telegram/web chat.
	if h.MaxContextWindow > 0 && h.OutputBase != "" {
		totalChars := 0
		for _, m := range messages {
			totalChars += len(m.Content)
		}
		estimatedTokens := totalChars / 4

		if estimatedTokens > h.MaxContextWindow/2 {
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Role == "user" && len(messages[i].Content) > 4000 {
					content := messages[i].Content

					// Tier 1: Mount as file in session workspace.
					fileDir := filepath.Join(h.OutputBase, taskID, "inputs")
					if err := os.MkdirAll(fileDir, 0755); err != nil {
						log.Printf("WARN: chat_harness: failed to create input dir: %v", err)
					}
					filename := fmt.Sprintf("large_input_%d.txt", time.Now().UnixMilli())
					filePath := filepath.Join(fileDir, filename)
					// audit NEW-MED-1: log write failure — previously silently
					// discarded, causing tool output to be silently lost.
					if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
						log.Printf("WARN: chat_harness: failed to write large input file: %v", err)
					}

					// Generate summary: head + tail + line count.
					lines := strings.Split(content, "\n")
					headEnd := 20
					if headEnd > len(lines) {
						headEnd = len(lines)
					}
					tailStart := len(lines) - 20
					if tailStart < headEnd {
						tailStart = headEnd
					}
					summary := fmt.Sprintf("[Large input mounted: %d chars, %d lines, ~%d tokens. Saved as: %s]\n",
						len(content), len(lines), estimatedTokens, filename)
					summary += "--- First 20 lines ---\n" + strings.Join(lines[:headEnd], "\n") + "\n"
					if tailStart < len(lines) && tailStart > headEnd {
						summary += fmt.Sprintf("... [%d lines omitted] ...\n", tailStart-headEnd)
						summary += "--- Last 20 lines ---\n" + strings.Join(lines[tailStart:], "\n")
					}
					summary += fmt.Sprintf("\n[Use read_file path=%s start_line=N end_line=M to read specific sections]", filename)

					// Tier 3: Auto-delegate if input exceeds full context window.
					if estimatedTokens > h.MaxContextWindow && h.Delegate != nil {
						intent := content
						if len(intent) > 500 {
							intent = intent[:250] + " ... " + intent[len(intent)-250:]
						}
						delegatePrompt := fmt.Sprintf(
							"Read and process the file at %s.\nUser intent (preview): %s\nFile summary: %s\n"+
								"Use read_file with start_line/end_line to read in batches. Analyze and respond to the user's request.",
							filePath, intent, summary)
						delTaskID, delErr := h.Delegate(delegatePrompt)
						if delErr == nil {
							emit(ChatEvent{Type: "tool_call", Data: "auto_delegated_to_orchestrator", Meta: map[string]any{
								"task_id": delTaskID,
								"reason":  "input exceeds context window",
								"tokens":  estimatedTokens,
							}})
							result := fmt.Sprintf(
								"Your input is large (~%d tokens, exceeds model context window of %d).\n"+
									"I've saved it to %s and delegated analysis to the orchestrator.\n"+
									"Task: %s\nUse /status %s to check progress.",
								estimatedTokens, h.MaxContextWindow, filename, delTaskID, delTaskID)
							emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": "auto_delegate"}})
							emit(ChatEvent{Type: "done"})
							return result, nil
						}
						// If delegate fails, fall through to Tier 1 (truncated chat).
					}

					// Tier 1: Replace the message with summary + file reference.
					messages[i].Content = summary
					emit(ChatEvent{Type: "steer", Data: fmt.Sprintf(
						"large input (%d tokens) mounted as file %s, message replaced with summary",
						estimatedTokens, filename)})
					break
				}
			}
		}
	}

	var finalText strings.Builder               // audit H1: was `var finalText string` with += (O(n²))
	recentSigs := make([]string, 0, 5)          // fallback windowed loop detection when sieve.Sieve is nil (Gap-3)
	toolFailErrors := make(map[string][]string) // per-tool recent error messages (capped at 5)
	noTextTurns := 0                            // consecutive turns with tool calls but no text output
	// D8 (2026-11-01): the no-text nudge must fire at most once per run, and
	// must stay silent while tools are landing results.
	//
	// It used to fire every 3 turns forever: `noTextTurns` was reset to 0 after
	// each fire, so on p22 (108 calls, all tool-only) it fired 36 times with
	// byte-identical text — `no text output for 3 consecutive turns`. A model
	// that has already been told cannot learn anything from being told again,
	// and a tool-driven trajectory never produces prose by design, so the
	// predicate was true for the whole run (iron law 32: an unsatisfiable /
	// always-true predicate is not a guard).
	noTextNudged := false                // latched once per run
	lastTurnToolsOK := false             // did the previous turn land >=1 non-empty tool result?
	overflowRetried := false             // Tier 2: ensure context overflow retry happens only once
	var lastToolMeta *store.ToolMetadata // Phase 3: last tool's structured metadata for truncation guard

	// ── Run counters (D8) ────────────────────────────────────────────
	// Every "did the cost work actually help?" question about p22 needed a
	// script over events.sse plus a hand count over the engine log, because
	// the harness emitted events but never summarised a run. These counters
	// make the verdict one line of `docker logs`, and they are hard counts, so
	// they do not depend on the scoring policy (which is still undecided).
	s := newRunStats(len(declaredArtifacts), len(declaredFields))
	execGuardMode, execStreakLimit := codeExecGuardSettings()
	eg := exploreGuard{writeTargets: make(map[string]struct{}), writeCounts: make(map[string]int)}

	log.Printf("[chat-harness] task=%s sieve_nil=%v turns=%d", taskID, h.Sieve == nil, maxTurns)

	// One structured line per run, on every exit path. Emitted from a defer so
	// a new early return can never silently skip the measurement.
	defer func() {
		s.emitSummary(h, taskID, err, &eg, contractSteers, contractDefectSteers)
	}()

	defer func() {
		if err == nil && finalText.Len() > 0 && query != "" {
			h.indexTurn(ctx, taskID, query, finalText.String())
		}
	}()

	for turn := 0; turn < maxTurns; turn++ {
		// Abort if the context was cancelled (e.g. client disconnect or shutdown).
		if ctx.Err() != nil {
			emit(ChatEvent{Type: "error", Data: fmt.Sprintf("context cancelled: %v", ctx.Err())})
			return finalText.String(), ctx.Err()
		}

		prevLen := finalText.Len()
		s.turns++
		turnToolsOK := false // reset per turn: did THIS turn land a usable tool result?

		req := providers.CompleteRequest{
			System:      system,
			User:        flattenChatMessages(messages),
			Model:       h.Model,
			MaxTokens:   maxOutputTokens,
			MCPServers:  []schemas.MCPServerDef{{Name: "core", Tools: s.offerTools(h)}},
			Attachments: h.Attachments,
		}

		// Stream chunk detector (per-turn, per-call): catches intra-call
		// repetitive output that tool-call-centric guards cannot see — e.g.
		// a weak model emitting dozens of identical reasoning/text chunks
		// without any tool calls. Inspired by Cordis thinking-loop-guard.
		resp, err := h.streamTurn(ctx, req, &finalText, emit)
		if err != nil {
			if errors.Is(err, stream.ErrRepetitiveOutput) {
				// Steer: undo this turn's partial text, inject guidance,
				// and retry. Non-latching — the turn continues normally.
				if finalText.Len() > prevLen {
					s := finalText.String()[:prevLen]
					finalText.Reset()
					finalText.WriteString(s)
				}
				emit(ChatEvent{Type: "loop_guard", Data: "repetitive streaming output detected"})
				emit(ChatEvent{Type: "steer", Data: "repetitive output detected, refocus"})
				messages = append(messages, ChatMessage{
					Role:    "user",
					Content: core.MarkerRepetitiveOutput,
				})
				continue
			}
			// Tier 2: Context overflow recovery — detect, truncate, retry once.
			// Reuses core.IsContextLimitError from spawner_utils.go. This catches
			// the case where the provider returns context_length_exceeded
			// (e.g., 4B model with 4K context receiving a large input that
			// slipped past Tier 1 estimation).
			if core.IsContextLimitError(err) && !overflowRetried {
				overflowRetried = true
				emit(ChatEvent{Type: "steer", Data: "context overflow detected, truncating input and retrying"})
				for i := len(messages) - 1; i >= 0; i-- {
					if messages[i].Role == "user" && len(messages[i].Content) > 4000 {
						content := messages[i].Content
						half := 2000
						if half > len(content)/2 {
							half = len(content) / 2
						}
						messages[i].Content = content[:half] +
							fmt.Sprintf("\n[... truncated: %d chars omitted due to context overflow ...]\n", len(content)-half*2) +
							content[len(content)-half:]
						break
					}
				}
				turn-- // retry same turn with truncated input
				continue
			}
			// Turn budget exhaustion: distinguish "our clock killed the
			// turn" from "the model/provider actually errored" so downstream
			// scoring doesn't mislabel a budget timeout as model_error.
			// See emitTimeoutError in chat_budget.go for the two paths and
			// the rate-limit-aware labelling.
			providerName := h.Provider.Name()
			if emitTimeoutError(emit, err, ctx, providerName) {
				return finalText.String(), err
			}
			emitRunError(emit, err, providerName)
			return finalText.String(), err
		}

		if len(resp.ToolCalls) == 0 {
			s.exitReason = "done"
			emit(ChatEvent{Type: "done"})
			return finalText.String(), nil
		}

		// Steer: if the model keeps making tool calls without producing any
		// visible text for the user, nudge it toward converging. This catches
		// "reasoning-only loops" where the model churns through tool calls
		// without ever generating a response.
		//
		// D8 (2026-11-01): latched. Two independent defects made this a no-op
		// on p22:
		//  1. `noTextTurns` was cleared after every fire ⇒ it re-armed every 3
		//     turns and fired 36 times in one run, all with the same string.
		//  2. The predicate is true for the whole run when the model works
		//     through tools instead of prose — the normal shape here, not a
		//     stall. `lastTurnToolsOK` suppresses the nudge in that case.
		// Net effect: at most one nudge per run, and only when tools are also
		// failing — i.e. the model really is spinning with nothing to show.
		if finalText.Len()-prevLen == 0 {
			noTextTurns++
		} else {
			noTextTurns = 0
		}
		if noTextTurns >= 3 {
			n := noTextTurns
			noTextTurns = 0
			if !noTextNudged && !lastTurnToolsOK {
				noTextNudged = true
				s.noTextSteers++
				emit(ChatEvent{Type: "steer", Data: fmt.Sprintf("no text output for %d consecutive turns", n)})
				messages = append(messages, ChatMessage{
					Role:    "user",
					Content: core.MarkerNoTextResponse,
				})
			}
		}

		// Phase 3: Truncation guardian — if the last tool result was truncated
		// and the model uses universal quantifiers, block and warn.
		if finalText.Len()-prevLen > 0 && h.Sieve != nil && lastToolMeta != nil {
			newText := finalText.String()[prevLen:]
			if ok, reason := h.Sieve.InspectTruncationArtifact(lastToolMeta, newText); !ok {
				emit(ChatEvent{Type: "truncation_violation", Data: reason})
			}
		}

		// Loop detection: log repetitive tool calls for observability.
		// Consequence is cache replay (in the execution loop below), not
		// force-termination — force-terminate causes small models to panic
		// (empty turns / transfer-to-human) instead of continuing.
		if h.Sieve != nil {
			interactions := make([]store.ToolInteraction, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				interactions[i] = store.ToolInteraction{ToolName: tc.Name, Arguments: tc.Arguments}
			}
			if ok, reason := h.Sieve.InspectToolCalls(taskID, "", interactions); !ok {
				emit(ChatEvent{Type: "loop_guard", Data: reason})
			}
		} else {
			// Gap-3: fallback windowed loop detection when sieve.Sieve is nil.
			// Maintains recent 5 signatures and flags if the current sig
			// appears >= 3 times in the window (replaces weak 1-hop prevSig).
			sig := toolCallSignature(resp.ToolCalls)
			if sig != "" {
				recentSigs = append(recentSigs, sig)
				if len(recentSigs) > 5 {
					recentSigs = recentSigs[len(recentSigs)-5:]
				}
				count := 0
				for _, s := range recentSigs {
					if s == sig {
						count++
					}
				}
				if count >= 3 {
					emit(ChatEvent{Type: "loop_guard", Data: "identical tool calls repeated (windowed fallback)"})
				}
			}
		}

		for i, tc := range resp.ToolCalls {
			s.recordTool(tc.Name)
			emit(ChatEvent{Type: "tool_call", Data: tc.Name, Meta: map[string]any{"args": tc.Arguments, "session_task_id": taskID}}) // not task_id (that means task_* on this stream); not role (the emit wrapper stamps role_id/role_name)

			h.checkPlanDrift(i, taskID, tc, emit)

			// Cache replay: if this exact read was executed before, return
			// the cached result instead of re-executing. Breaks repetition
			// loops without force-terminating the conversation.
			if h.Sieve != nil {
				if cached, found := h.Sieve.GetCachedResult(taskID, tc.Name, tc.Arguments); found {
					log.Printf("[chat-harness] cache_replay HIT task=%s tool=%s", taskID, tc.Name)
					result := cached + "\n\n[CACHED REPLAY — same query as before; no database changes since last call.]"
					emit(ChatEvent{Type: "cache_replay", Data: tc.Name})
					emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": tc.Name, "cached": true}})
					messages = append(messages, ChatMessage{Role: "tool", Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, result)})
					continue
				}
			}

			if h.beforeToolCall(tc, taskID, emit, &messages) {
				continue
			}

			var rule toolAfterRule
			result, derr := h.execTool(ctx, tc.Name, tc.Arguments, taskID)
			if derr != nil {
				// Track error messages (not just count) so we can inject them
				// as pattern evidence after repeated failures. This lets the
				// model see WHY it keeps failing and avoid the pattern itself,
				// instead of getting a generic "stop using this tool" with no
				// actionable information.
				errMsg := derr.Error()
				if r := []rune(errMsg); len(r) > 200 { // audit NEW-3: rune-aware truncation avoids splitting multibyte UTF-8
					errMsg = string(r[:200]) + "..."
				}
				toolFailErrors[tc.Name] = append(toolFailErrors[tc.Name], errMsg)
				if len(toolFailErrors[tc.Name]) > 5 {
					toolFailErrors[tc.Name] = toolFailErrors[tc.Name][len(toolFailErrors[tc.Name])-5:]
				}
				failCount := len(toolFailErrors[tc.Name])
				emit(ChatEvent{Type: "tool_error", Data: derr.Error(), Meta: map[string]any{"tool": tc.Name, "fail_count": failCount}})
				if failCount >= 3 {
					result = fmt.Sprintf("[TOOL %s HAS FAILED %d TIMES] Recent errors:\n%s\nDo not repeat the same approach. Try a different strategy or answer directly.", tc.Name, failCount, strings.Join(toolFailErrors[tc.Name], "\n"))
				} else {
					result = fmt.Sprintf(core.ToolFailOnceFmt, tc.Name, derr.Error())
				}
			} else {
				toolFailErrors[tc.Name] = nil
				// Read the raw result before any steer/externalization
				// rewrites it: "the tool actually worked" is what exempts the
				// no-text nudge, and a failure message is also non-empty.
				if strings.TrimSpace(result) != "" {
					turnToolsOK = true
				}
				if h.Sieve != nil {
					switch toolResultPolicyFor(tc.Name, tc.Arguments) {
					case alwaysFreshResult:
						// The answer changes on its own: execute fresh (already
						// done), keep it out of the cache, and do NOT touch the
						// cache — a poll says nothing about whether cached reads
						// are still valid.
					case actionResult:
						h.Sieve.ClearResultCache(taskID)
						result = "[ACTION COMPLETED] " + result
					default:
						h.Sieve.CacheToolResult(taskID, tc.Name, tc.Arguments, result)
						if h.Sieve.CheckResultRepetition(taskID, result) {
							result = "[RESULT REPETITION] You are receiving the same result from different queries. The data is not changing. Stop re-querying and provide a direct answer based on the information you already have.\n\n" + result
							emit(ChatEvent{Type: "result_repetition", Data: tc.Name})
						}
					}
				}

				// ── Artifact Externalization ──────────────────────────
				// Large tool results are written to a file and replaced with
				// a skeleton + path reference, keeping context windows bounded
				// while preserving full recall via read_file + search_archive.
				// read_file results are exempt below a higher ceiling: they are
				// the recall path itself (see shouldExternalize).
				if h.shouldExternalize(tc.Name, result) {
					result = h.externalizeArtifact(result, tc.Name, taskID)
				}

				// ── Write-repetition (D7) + no-output exploration (P1) ──
				// Both guards live in afterToolCall: the write-repetition steer
				// was measured on 026 (11 successive writes to one path, every
				// signature guard silent), and the exploration streak is the
				// only thing that can see execute_code/search_archive, which
				// never yield a write target. Extracted so the escalate arm can
				// stop the batch or the run without growing Run (Q6-1).
				//
				// Placed after externalization so the steer is never swapped
				// out for a skeleton, and inside the success branch so a
				// failed call (which changed nothing) does not count.
				rule = h.afterToolCall(tc, &result, &eg, execGuardMode, execStreakLimit, declaredArtifacts, emit)
			}

			// Phase 2: extract structured metadata from tool result for
			// truncation guardian (Phase 3). Tries to parse result as JSON.
			if result != "" {
				var m map[string]any
				if json.Unmarshal([]byte(result), &m) == nil {
					lastToolMeta = core.ExtractMeta(m)
				}
			}
			// Aligns with Hermes steer semantics: the model sees the
			// correction after all tool results, maximizing its weight.
			isLast := i == len(resp.ToolCalls)-1
			if isLast {
				h.SteerLock.Lock()
				if h.PendingSteer != "" {
					result += "\n\n" + h.PendingSteer
					h.PendingSteer = ""
				}
				h.SteerLock.Unlock()

				// ── Completion contract steer (stage 2c) ──────────────
				// Emitted at most once per run, on the last tool result of a
				// turn — the same placement as PendingSteer, because that is
				// where the model looks next.
				//
				// This does not change the exit condition. The model still
				// decides to stop; the steer only tells it that stopping is now
				// justified, since otherwise the only thing that ever says
				// "done" is the turn budget.
				//
				// SessionRoot == "" is the legacy layout (artifacts under
				// outputs/<taskID>), where these relative names are not rooted
				// at any workspace. os.Stat would resolve them against the
				// process working directory — the deploy directory — so a
				// colliding name announces "all artifacts exist" over an empty
				// workspace while everything else reads present=0 forever.
				// Neither answer is about this run, so neither is worth having.
				if h.contractArmed() && h.SessionRoot != "" {
					_, missing := core.WorkspaceArtifactsComplete(h.SessionRoot, declaredArtifacts)
					defects := h.artifactDefects(ctx, taskID, emit)
					if contractSteers == 0 && len(declaredArtifacts) > 0 && missing == 0 && len(defects) == 0 && (h.DelegateStatus == nil || h.isDelegatedTaskTerminal(taskID)) {
						contractSteers++
						// The event always fires: "when would the steer have
						// fired" is the observation arm C needs, and an absent
						// event cannot be told apart from a contract that was
						// never satisfiable.
						emit(ChatEvent{Type: EvArtifactsComplete, Meta: map[string]any{
							"declared": len(declaredArtifacts),
							"mode":     completionContract.String(),
						}})
						if marker := core.CompletionContractMarker(completionContract); marker != "" {
							result += "\n\n" + marker
						}
					} else if len(defects) > 0 && contractDefectSteers < maxContractDefectSteers {
						// Present but wrong is not done. Say what is wrong in the
						// same slot as the done steer, because that is where the
						// model looks next, and do not send the done sentence
						// (→113: a green light over an artifact the scorer will
						// reject is worse than no signal at all).
						contractDefectSteers++
						emit(ChatEvent{Type: EvArtifactsIncomplete, Meta: map[string]any{
							"declared": len(declaredArtifacts),
							"missing":  missing,
							"defects":  len(defects),
							"mode":     completionContract.String(),
						}})
						if marker := core.ContractDefectsMarker(defects, h.contract.lastExtra); marker != "" {
							result += "\n\n" + marker
						}
					}
				}
			}

			emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": tc.Name}})
			messages = append(messages, ChatMessage{Role: "tool", Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, result)})

			// Escalate arm only: level 2 stops the rest of this turn's
			// exploration, unless the batch already queued a write (stopping
			// there would swallow the very action the steer asks for). Level 3
			// ends the run; the partial is kept with its own marker, because
			// this is a deliberate ceiling, not a provider failure.
			if rule.stopBatch && !h.batchHasWrite(resp.ToolCalls[i+1:]) {
				skipRemainingCalls(resp.ToolCalls[i+1:], emit, &messages)
				break
			}
			if rule.endRun {
				s.exitReason = "exploration_exhausted"
				emit(ChatEvent{Type: "error", Data: fmt.Sprintf("exploration ceiling: %d calls without a write", eg.streak), Meta: map[string]any{"exit_reason": "exploration_exhausted", "cap": eg.streak}})
				return markExplorationTruncated(finalText.String(), eg.streak),
					fmt.Errorf("exploration ceiling reached (%d calls without a write): %w", eg.streak, ErrExplorationExhausted)
			}
		}
		// Carry this turn's tool health into the next iteration, where the
		// no-text check decides whether the model is spinning or working.
		lastTurnToolsOK = turnToolsOK
	}

	s.exitReason = "max_turns"
	emit(ChatEvent{Type: "error", Data: fmt.Sprintf("max turns (%d) exceeded", maxTurns), Meta: map[string]any{"exit_reason": "max_turns", "cap": maxTurns}})
	return markTruncated(finalText.String(), maxTurns), fmt.Errorf("max turns (%d) exceeded: %w", maxTurns, ErrMaxTurnsExceeded)
}

// streamTurn runs one provider call for Run: content is appended to finalText
// and streamed as "delta" events, thinking is streamed as "thinking" events and
// kept out of finalText (it is internal reasoning, not the response). The stream
// is aborted when the chunk detector fires (handled by the caller as a steer) or
// when the client disconnects. The thinking extractor is flushed afterwards so
// the tail of the response is never lost.
func (h *ChatHarness) streamTurn(ctx context.Context, req providers.CompleteRequest, finalText *strings.Builder, emit func(ChatEvent) error) (*providers.ProviderResponse, error) {
	chunkDetector := stream.NewStreamChunkDetector()
	thinkExtractor := &thinkingExtractor{}
	resp, err := h.Provider.StreamComplete(ctx, req, func(chunk string) error {
		if chunk != "" {
			if e := chunkDetector.Check(chunk); e != nil {
				return e // abort stream; handled below as steer, not fatal
			}
			// Separate thinking (imd/}) from content.
			// Thinking is emitted as "thinking" events and NOT written
			// to finalText — it's internal reasoning, not the response.
			content, thinking := thinkExtractor.Process(chunk)
			if content != "" {
				finalText.WriteString(content)
				if e := emit(ChatEvent{Type: "delta", Data: content}); e != nil {
					return e // audit H5: abort stream if client disconnected
				}
			}
			if thinking != "" {
				if e := emit(ChatEvent{Type: "thinking", Data: thinking}); e != nil {
					return e
				}
			}
		}
		return nil
	})
	// Flush any remaining buffered content from the thinking extractor.
	if resp != nil {
		content, thinking := thinkExtractor.Flush()
		if content != "" {
			finalText.WriteString(content)
			emit(ChatEvent{Type: "delta", Data: content})
		}
		if thinking != "" {
			emit(ChatEvent{Type: "thinking", Data: thinking})
		}
	}
	return resp, err
}

func (h *ChatHarness) indexTurn(ctx context.Context, sessionID, userQuery, assistantResponse string) {
	if h.Archive == nil || h.Embed == nil {
		return
	}
	embCtx, embCancel := context.WithTimeout(ctx, 30*time.Second) // audit L-MED-7: derive timeout from parent context to avoid goroutine leak
	defer embCancel()
	vec, embErr := h.Embed.Embed(embCtx, userQuery)
	if embErr != nil {
		return
	}
	if err := h.Archive.Append(core.MemoryItem{
		TaskID:    "chat_" + sessionID,
		NodeID:    "chat_" + sessionID + "_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Intent:    userQuery,
		Summary:   assistantResponse,
		Embedding: vec,
		Timestamp: time.Now(),
	}); err != nil {
		log.Printf("WARN: chat_harness: failed to archive memory item: %v", err)
	}

	// P1-8 (2026-10-10): Path A — feed the turn into the core.SegmentManager
	// for topic segmentation. The core.SegmentManager decides whether the item
	// stays in the current segment or seals it and starts a new one.
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
	if h.Segments != nil {
		h.Segments.AddItem(core.MemoryItem{
			TaskID:    "chat_" + sessionID,
			NodeID:    "chat_" + sessionID + "_" + strconv.FormatInt(time.Now().UnixNano(), 36),
			Intent:    userQuery,
			Summary:   assistantResponse,
			Timestamp: time.Now(),
		}, "chat_"+sessionID)
	}
}
