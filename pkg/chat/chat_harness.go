package chat

import (
	"context"
	"log"
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
	// Delegate hands off heavy multi-step work to the orchestrator and receives
	// the parent's remaining sub-agent turn cap. Returns the orchestrator task id.
	Delegate func(prompt string, maxTurnsCap int) (string, error)
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
	// OnFileRead is called when a read_file call succeeded, with the file's base
	// name. Deliberately blind to *which* task the file belongs to: the harness
	// knows the front-desk prompt's contract, but the wake entry is the only
	// thing that knows which finished task a read should count for, so the
	// matching happens there (deliverPendingWake).
	//
	// It exists because OnTerminalTaskSeen turned out to be a bad proxy for "the
	// model has the result": formatTaskStatus never carries artifact content, and
	// measured over 283 archived firings the turn had read no declared artifact
	// by the time the wake was dropped in 137 of them (48%) — at most 146 (52%)
	// had touched one at all. See pkg/api/wake_content_suppress.go wakeDropDecision.
	//
	// Optional. Installed per request (it closes over the session id), so it is
	// deliberately not carried by cloneHarnessForRequest.
	OnFileRead   func(basename string)
	ScheduleExec func(action string, args map[string]any) (string, error)
	MaxTurns     int
	// WakeReserve and WakeReserveRatio keep part of the parent turn budget out
	// of delegated work. Zero values use the package defaults.
	WakeReserve      int
	WakeReserveRatio float64
	currentTurn      int
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
	// declaredContracts carries wake-only type and optionality constraints; nil on ordinary prompt-parsed turns.
	declaredContracts []schemas.ArtifactContract
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
	// See docs/completed/2026-10-26/STEP_PLAN_MODE_DESIGN.md §10.3.
	PendingSteer string
	SteerLock    sync.Mutex

	fileReadCalls     int // delegation drift: per-turn read-only tool call count
	delegationSteered bool

	// ReadinessGateSoft (ADDED 2026-10-10): when true, softens the
	// delegation readiness gate language from "NEVER blindly delegate"
	// to a suggestion. Weak models (≤7B) over-apply the hard rule and
	// ask excessive clarifying questions instead of acting. Default
	// false = strict (current behavior for strong models).
	// See docs/completed/TOOL_LOOP_GUARDRAILS_PLAN.md §0.8.
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
	Segments       *core.SegmentManager
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
	defer func() { h.currentTurn = 0 }()

	run, earlyOutput, delegated := h.prepareRun(ctx, taskID, history, emit)
	if delegated {
		return earlyOutput, nil
	}
	run.initLoop()

	// One structured line per run, on every loop exit path.
	defer func() {
		run.stats.emitSummary(h, taskID, err, &run.eg, run.contractSteers, run.contractDefectSteers)
	}()
	defer func() {
		if err == nil && run.finalText.Len() > 0 && run.query != "" {
			h.indexTurn(ctx, taskID, run.query, run.finalText.String())
		}
	}()

	return run.execute()
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
	// See docs/completed/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
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
