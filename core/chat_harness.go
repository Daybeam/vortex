package core

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
	"github.com/daybeam/vortex/core/skeleton"
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
	Archive    *ContextArchive
	MemoryBank *store.MemoryBankStore
	Embed      EmbeddingClient
	OutputBase string
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
	MaxTurns       int
	// MaxOutputTokens caps the output tokens per LLM call. 0 = default 2048.
	// (ADDED 2026-10-28: previously hardcoded to 2048 with no config wiring
	// — harness-bench report §4.1.)
	MaxOutputTokens int
	// SessionRoot is the workspace root for path resolution in core tools.
	// When non-empty, a SafePathValidator is passed to HandleCoreTool so
	// relative paths resolve against the workspace instead of outputs/<taskID>.
	// When empty, behavior is unchanged (backward compatible).
	// (ADDED 2026-10-28: harness-bench report §4.2.)
	SessionRoot string
	// Sieve is an optional repetition detector. If nil, a lightweight
	// signature-based fallback is used instead.
	Sieve *Sieve
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

	// ReadinessGateSoft (ADDED 2026-10-10): when true, softens the
	// delegation readiness gate language from "NEVER blindly delegate"
	// to a suggestion. Weak models (≤7B) over-apply the hard rule and
	// ask excessive clarifying questions instead of acting. Default
	// false = strict (current behavior for strong models).
	// See eval-kit/docs/28-engine-path-analysis.md §2 claim ②.
	ReadinessGateSoft bool

	// MaxContextWindow is the model's context window size (in tokens),
	// from the provider config. Used for input size estimation: when
	// the user's input exceeds 50% of this, it is mounted as a file
	// with a summary; when it exceeds 100%, it is auto-delegated to
	// the orchestrator. 0 = no estimation (legacy behavior).
	MaxContextWindow int

	// Segments (ADDED 2026-10-10): optional topic segment manager for
	// chat memory. When non-nil, each indexed turn is also processed by
	// the SegmentManager for topic segmentation (STAY/SEAL decisions).
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
	Segments *SegmentManager
}

// thinkingExtractor detects imd/} tags in a chunk stream and separates
// thinking content from regular content. Handles tags split across chunks.
// Used by ChatHarness to emit "thinking" events separately from "delta" events,
// enabling Telegram/web clients to display reasoning collapsibly.
type thinkingExtractor struct {
	inThinking bool
	pending    string // buffer for partial tag detection
}

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// Process takes a chunk and returns (content, thinking) strings.
// content should be emitted as "delta", thinking as "thinking".
// Both may be empty if the chunk is buffered for tag detection.
func (te *thinkingExtractor) Process(chunk string) (content, thinking string) {
	te.pending += chunk
	for len(te.pending) > 0 {
		if !te.inThinking {
			idx := strings.Index(te.pending, thinkOpenTag)
			if idx == -1 {
				// No tag found. Keep last (len-1) chars as pending
				// in case the tag is split across chunks.
				safe := len(te.pending) - (len(thinkOpenTag) - 1)
				if safe <= 0 {
					return content, thinking
				}
				content += te.pending[:safe]
				te.pending = te.pending[safe:]
			} else {
				if idx > 0 {
					content += te.pending[:idx]
				}
				te.pending = te.pending[idx+len(thinkOpenTag):]
				te.inThinking = true
			}
		} else {
			idx := strings.Index(te.pending, thinkCloseTag)
			if idx == -1 {
				safe := len(te.pending) - (len(thinkCloseTag) - 1)
				if safe <= 0 {
					return content, thinking
				}
				thinking += te.pending[:safe]
				te.pending = te.pending[safe:]
			} else {
				if idx > 0 {
					thinking += te.pending[:idx]
				}
				te.pending = te.pending[idx+len(thinkCloseTag):]
				te.inThinking = false
			}
		}
	}
	return content, thinking
}

// Flush returns any remaining buffered content. Call at the end of the stream.
func (te *thinkingExtractor) Flush() (content, thinking string) {
	if te.inThinking {
		thinking = te.pending
	} else {
		content = te.pending
	}
	te.pending = ""
	return content, thinking
}

func (h *ChatHarness) ToolDefinitions() []schemas.ToolDefinition {
	// Copy before mutating: CoreToolDefinitions() returns a shared package-level
	// var (audit fix P1). ToolDefinitions() mutates Description fields when
	// SessionRoot is set, so it must operate on a copy.
	src := CoreToolDefinitions()
	tools := make([]schemas.ToolDefinition, len(src))
	copy(tools, src)
	// Filter out decompress — it requires a FoldEngine which the ChatHarness
	// doesn't have. decompress is only available to subagents via the Spawner.
	filtered := tools[:0]
	for _, t := range tools {
		if t.Name != "decompress" {
			filtered = append(filtered, t)
		}
	}
	tools = filtered
	// When SessionRoot is set, adapt tool descriptions to reflect the real
	// workspace root and clarify execute_code is not a shell. (harness-bench
	// report §3 defect 3: descriptions contradicted actual permissions.)
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
					"PREFER execute_code over multiple read_file calls when:\n" +
					"- Counting functions/lines/patterns across many files\n" +
					"- Extracting structured data from logs/JSON/CSV\n" +
					"- Comparing or aggregating file contents\n" +
					"One script replaces N read_file calls and saves Nx context."
			}
		}
	}
	tools = append(tools, schemas.ToolDefinition{
		Name: "delegate_to_orchestrator",
		Description: "Delegate a complex, multi-step task to the orchestrator (role-based subagent execution). " +
			"USE THIS when the request spans several files/sources/sub-questions, needs several independent " +
			"tool rounds, needs a specialized role, or is long enough that the user would rather get a task id " +
			"now and follow up. Prefer execute_code for anything one script can finish. " +
			"Returns immediately with a task id and the task's current status; the work continues in the " +
			"background, and this session is resumed automatically when it finishes so the result can be " +
			"delivered then. Use orchestrator_task_status only when the answer cannot wait for that. " +
			"READINESS GATE (mandatory): Before calling this tool, verify the task is fully specified — " +
			"all required parameters present, no algorithmic ambiguity, no path/identifier contradictions, " +
			"and the expected outcome is concrete. If ANY of these is unclear, ask the user clarifying " +
			"questions FIRST instead of delegating an ambiguous task. Delegating a vague prompt pollutes " +
			"the downstream weak-model executor and wastes retry budget.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type": "string",
					"description": "Fully-specified task description to delegate. MUST include: (1) the concrete objective, " +
						"(2) any required inputs/paths/identifiers, (3) the expected output format or success criteria. " +
						"Do NOT delegate prompts containing placeholders like <TBD>, unspecified file paths, or unresolved " +
						"parameter choices — clarify with the user first.",
				},
				"readiness_self_check": map[string]any{
					"type": "boolean",
					"description": "Set to true to confirm you have verified: no missing parameters, no algorithmic ambiguity, " +
						"no path contradictions. Omit or set false only if you have already asked the user and received " +
						"clarification. This flag is the gateway's audit signal — delegating without it true is a protocol violation.",
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
	return tools
}

// buildSystem composes the system prompt from context-tree and memory-bank state.
func (h *ChatHarness) buildSystem(query, taskID string) string {
	var b strings.Builder

	// Role system prompt injection — mirrors prompt_assembler.go:66-72.
	// When a role is set, its Instruction defines the persona; the base
	// prompt below provides safety guardrails on top.
	if h.Role != nil && h.Role.Instruction != "" {
		b.WriteString(fmt.Sprintf(ChatHarnessRolePrefix, h.Role.Name, h.Role.Instruction))
	}

	b.WriteString(ChatHarnessDefaultIdentity)
	b.WriteString(ChatHarnessToolsetDesc)
	b.WriteString(ChatHarnessUsageGuidance)

	// Positive triggers first, prohibitions second. See ChatHarnessWhenToDelegate
	// for the measurement that made the ordering matter.
	b.WriteString(ChatHarnessWhenToDelegate)

	// ── Readiness Gate (Two-Tier Clarification Design, Module 1) ──────────────
	// Hard rule: the strong model (this harness) is the user-facing gateway. It
	// MUST perform Pre-codification Readiness Assessment before delegating to the
	// downstream weak-model orchestrator. Delegating an ambiguous task pollutes
	// the executor and burns retry budget on garbage inputs. See
	// docs/architecture/TWO_TIER_CLARIFICATION_AND_READINESS_DESIGN.md
	b.WriteString("\n## DELEGATION READINESS GATE (HARD RULE)\n")
	b.WriteString(ChatHarnessDelegatePrefix)
	b.WriteString(ChatHarnessDelegateRule1)
	b.WriteString(ChatHarnessDelegateRule2)
	b.WriteString(ChatHarnessDelegateRule3)
	b.WriteString("4. The expected outcome / success criteria is explicit.\n")
	if h.ReadinessGateSoft {
		b.WriteString("If any of these are unclear, consider asking the user for clarification before delegating.\n")
	} else {
		b.WriteString("If ANY of these fail, ask the user clarifying questions FIRST. NEVER blindly delegate a vague task.\n")
	}

	if h.Archive != nil {
		if forest, err := h.Archive.SearchForest(query, h.Embed, 5, 0.9, "", taskID); err == nil && len(forest.Nodes) > 0 {
			b.WriteString("\n## Relevant prior context\n")
			for _, n := range forest.Nodes {
				if n.Summary != "" {
					b.WriteString("- " + n.Summary + "\n")
				}
			}
		}
	}
	if h.MemoryBank != nil {
		if mb, err := h.MemoryBank.Load(); err == nil {
			if mb.ProductContext != "" {
				b.WriteString("\n## Product context\n" + mb.ProductContext + "\n")
			}
			if len(mb.ActiveContext.Goals) > 0 {
				b.WriteString("Goals: " + strings.Join(mb.ActiveContext.Goals, "; ") + "\n")
			}
			// User profile injection — helps the gateway agent disambiguate
			// vague intents and delegate precisely based on user expertise/preferences.
			if len(mb.UserProfile.Expertise) > 0 || len(mb.UserProfile.Preferences) > 0 || mb.UserProfile.CommunicationStyle != "" {
				b.WriteString("\n## User Profile\n")
				if len(mb.UserProfile.Expertise) > 0 {
					b.WriteString("Expertise: " + strings.Join(mb.UserProfile.Expertise, ", ") + "\n")
				}
				if len(mb.UserProfile.Preferences) > 0 {
					b.WriteString("Preferences: " + strings.Join(mb.UserProfile.Preferences, ", ") + "\n")
				}
				if mb.UserProfile.CommunicationStyle != "" {
					b.WriteString("Communication style: " + mb.UserProfile.CommunicationStyle + "\n")
				}
			}
		}
	}
	return b.String()
}

// Run executes the agent loop and streams events via emit. It returns the
// final assistant text (for persistence) and any error.
func (h *ChatHarness) Run(ctx context.Context, taskID string, history []ChatMessage, emit func(ChatEvent) error) (output string, err error) {
	maxTurns := h.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 6
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

	system := h.buildSystem(query, taskID)
	tools := h.ToolDefinitions()
	mcpServers := []schemas.MCPServerDef{{Name: "core", Tools: tools}}

	messages := make([]ChatMessage, 0, len(history)+maxTurns)
	if h.Memory != nil {
		history = h.Memory.Process(ctx, taskID, history)
	}
	messages = append(messages, history...)

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
	recentSigs := make([]string, 0, 5)          // fallback windowed loop detection when Sieve is nil (Gap-3)
	toolFailErrors := make(map[string][]string) // per-tool recent error messages (capped at 5)
	noTextTurns := 0                            // consecutive turns with tool calls but no text output
	// writeCounts counts successful writes per (tool, normalized path) inside
	// this turn. Signature-based guards cannot see a rewrite (the content
	// changes every time), so the target path is the only stable key. D7.
	writeCounts := make(map[string]int)
	overflowRetried := false             // Tier 2: ensure context overflow retry happens only once
	var lastToolMeta *store.ToolMetadata // Phase 3: last tool's structured metadata for truncation guard

	log.Printf("[chat-harness] task=%s sieve_nil=%v turns=%d", taskID, h.Sieve == nil, maxTurns)

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

		req := providers.CompleteRequest{
			System:      system,
			User:        flattenChatMessages(messages),
			Model:       h.Model,
			MaxTokens:   maxOutputTokens,
			MCPServers:  mcpServers,
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
					Content: MarkerRepetitiveOutput,
				})
				continue
			}
			// Tier 2: Context overflow recovery — detect, truncate, retry once.
			// Reuses isContextLimitError from spawner_utils.go. This catches
			// the case where the provider returns context_length_exceeded
			// (e.g., 4B model with 4K context receiving a large input that
			// slipped past Tier 1 estimation).
			if isContextLimitError(err) && !overflowRetried {
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
			emit(ChatEvent{Type: "error", Data: err.Error()})
			return finalText.String(), err
		}

		if len(resp.ToolCalls) == 0 {
			emit(ChatEvent{Type: "done"})
			return finalText.String(), nil
		}

		// Steer: if the model keeps making tool calls without producing any
		// visible text for the user, nudge it toward converging. This catches
		// "reasoning-only loops" where the model churns through tool calls
		// without ever generating a response. Non-latching: the turn continues
		// normally after the nudge.
		if finalText.Len()-prevLen == 0 {
			noTextTurns++
		} else {
			noTextTurns = 0
		}
		if noTextTurns >= 3 {
			emit(ChatEvent{Type: "steer", Data: fmt.Sprintf("no text output for %d consecutive turns", noTextTurns)})
			messages = append(messages, ChatMessage{
				Role:    "user",
				Content: MarkerNoTextResponse,
			})
			noTextTurns = 0
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
			// Gap-3: fallback windowed loop detection when Sieve is nil.
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
			emit(ChatEvent{Type: "tool_call", Data: tc.Name, Meta: map[string]any{"args": tc.Arguments}})

			// Plan drift detection (#4 + #10, ADDED 2026-10-10):
			// Check the first tool call in the batch against the step
			// plan. If drift is detected, set PendingSteer — it will be
			// appended to the last tool result below.
			// See docs/plan/STEP_PLAN_MODE_DESIGN.md §10.3.
			if i == 0 && h.Sieve != nil && h.PendingSteer == "" {
				if msg := h.Sieve.DetectPlanDrift(taskID, tc); msg != "" {
					h.SteerLock.Lock()
					h.PendingSteer = msg
					h.SteerLock.Unlock()
					emit(ChatEvent{Type: "plan_drift", Data: tc.Name})
				}
			}

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
					result = fmt.Sprintf(ToolFailOnceFmt, tc.Name, derr.Error())
				}
			} else {
				toolFailErrors[tc.Name] = nil
				if h.Sieve != nil {
					if isPollerTool(tc.Name) {
						// poller: always execute fresh, don't disturb cache
					} else if !isReadTool(tc.Name) {
						h.Sieve.ClearResultCache(taskID)
						result = "[ACTION COMPLETED] " + result
					} else {
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

				// ── Write-side repetition guard (D7) ─────────────────
				// A rewrite carries different `content` every time, so no
				// signature-based guard can see it: cache replay and
				// CheckResultRepetition are read-only, InspectToolCalls needs
				// identical arguments, and the no-text nudge does not latch.
				// Key on the target path instead. Measured on 026
				// (2026-10-30): 11 successive writes to one path, every guard
				// silent, budget exhausted. See T2-STALL-RESUME §16.3.
				//
				// Placed after externalization so the steer is never swapped
				// out for a skeleton, and inside the success branch so a
				// failed write (which overwrote nothing) does not count.
				if target := h.writeTargetOf(tc.Name, tc.Arguments); target != "" {
					if n, steer := writeGuardSteer(writeCounts, tc.Name, target); steer != "" {
						emit(ChatEvent{Type: "repeated_write", Data: target, Meta: map[string]any{"tool": tc.Name, "count": n}})
						result += "\n\n" + steer
					}
				}
			}

			// Phase 2: extract structured metadata from tool result for
			// truncation guardian (Phase 3). Tries to parse result as JSON.
			if result != "" {
				var m map[string]any
				if json.Unmarshal([]byte(result), &m) == nil {
					lastToolMeta = extractMeta(m)
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
			}

			emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": tc.Name}})
			messages = append(messages, ChatMessage{Role: "tool", Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, result)})
		}
	}

	emit(ChatEvent{Type: "error", Data: fmt.Sprintf("max turns (%d) exceeded", maxTurns)})
	return finalText.String(), fmt.Errorf("max turns (%d) exceeded", maxTurns)
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
	if err := h.Archive.Append(MemoryItem{
		TaskID:    "chat_" + sessionID,
		NodeID:    "chat_" + sessionID + "_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Intent:    userQuery,
		Summary:   assistantResponse,
		Embedding: vec,
		Timestamp: time.Now(),
	}); err != nil {
		log.Printf("WARN: chat_harness: failed to archive memory item: %v", err)
	}

	// P1-8 (2026-10-10): Path A — feed the turn into the SegmentManager
	// for topic segmentation. The SegmentManager decides whether the item
	// stays in the current segment or seals it and starts a new one.
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
	if h.Segments != nil {
		h.Segments.AddItem(MemoryItem{
			TaskID:    "chat_" + sessionID,
			NodeID:    "chat_" + sessionID + "_" + strconv.FormatInt(time.Now().UnixNano(), 36),
			Intent:    userQuery,
			Summary:   assistantResponse,
			Timestamp: time.Now(),
		}, "chat_"+sessionID)
	}
}

// delegateReceipt is what the model sees right after delegating. It must answer
// three questions in one shot: what was created, is it actually running, and
// what do I do next to get the answer.
//
// The "next" line used to say "collect the result, do not just tell the user it
// is running". That was true when it was written — the turn was the only thing
// that could produce an answer — and it caused a 300 s turn to be spent polling
// (trial 026: 34 status calls in 40 turns). Now that the completion callback
// starts a follow-up turn (chat_followup.go, segment ④), the honest instruction
// is the opposite one: replying now is correct, and cheaper, because the result
// gets delivered by that second turn. The loop warning stays — the failure mode
// has not disappeared, only its necessity.
func (h *ChatHarness) delegateReceipt(id string) string {
	var b strings.Builder
	b.WriteString("delegated task_id=" + id + " status=accepted\n")
	b.WriteString("The orchestrator is running it in the background, as its own task with its own plan.\n")
	if h.DelegateStatus != nil {
		if st, ok := h.DelegateStatus(id, 0); ok && st != nil {
			if s, _ := st["status"].(string); s != "" {
				b.WriteString("current status: " + s + "\n")
			}
		}
		b.WriteString("next: reply to the user now with the state you have — this session is resumed " +
			"automatically when the task finishes, and the result is delivered then. " +
			"If the answer is useless without the result, call orchestrator_task_status(task_id=\"" + id +
			"\", wait_seconds=20) instead; call it at most a couple of times and never loop on it.\n")
	} else {
		b.WriteString("next: the result arrives as a task_callback event in this session.\n")
	}
	return b.String()
}

func (h *ChatHarness) execTool(ctx context.Context, name string, args map[string]any, taskID string) (string, error) {
	if name == "delegate_to_orchestrator" {
		if h.Delegate == nil {
			return "", fmt.Errorf("delegate is not available")
		}
		prompt, ok := args["prompt"].(string)
		if !ok || prompt == "" {
			return "", fmt.Errorf("delegate_to_orchestrator: missing or empty 'prompt' argument")
		}
		id, err := h.Delegate(prompt)
		if err != nil {
			return "", err
		}
		// Return a receipt, not just an id. The tool is asynchronous by design
		// (the user wanted a task id they can follow up on), but "Delegated to
		// orchestrator task X" alone leaves the model with nothing to act on:
		// it cannot see progress and cannot see the result, while execute_code
		// hands back its stdout in the same turn. That asymmetry — not the
		// model's judgement — is why delegation measured 0/104.
		return h.delegateReceipt(id), nil
	}
	if name == "orchestrator_task_status" {
		if h.DelegateStatus == nil {
			return "", fmt.Errorf("orchestrator_task_status: task status is not wired in this deployment")
		}
		id, _ := args["task_id"].(string)
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("orchestrator_task_status: 'task_id' is required")
		}
		wait := 0.0
		if w, ok := args["wait_seconds"].(float64); ok && w > 0 {
			wait = w
			if wait > 60 {
				wait = 60 // a chat turn is waiting; do not let the model park it for minutes
			}
		}
		st, ok := h.DelegateStatus(id, time.Duration(wait*float64(time.Second)))
		if !ok || st == nil {
			return "", fmt.Errorf("orchestrator_task_status: task %s not found", id)
		}
		return formatTaskStatus(id, st), nil
	}
	if name == "search_archive" {
		query, _ := args["query"].(string)
		limit := 5
		if l, ok := args["limit"].(float64); ok && int(l) > 0 && int(l) <= 10 {
			limit = int(l)
		}
		if query == "" {
			return "", fmt.Errorf("search_archive: query is required")
		}
		if h.Archive == nil {
			return "", fmt.Errorf("search_archive: archive not available")
		}
		forest, err := h.Archive.SearchForest(query, h.Embed, limit, 0.9, taskID, taskID)
		if err != nil {
			return "", fmt.Errorf("search_archive: %v", err)
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d matching items:\n\n", len(forest.Nodes)))
		for i, n := range forest.Nodes {
			sb.WriteString(fmt.Sprintf("## %d. [%s] %s\n", i+1, n.NodeID, n.Intent))
			if n.Summary != "" {
				sb.WriteString(n.Summary + "\n\n")
			}
			// If this is an externalized artifact, show the file path for
			// direct read_file recall.
			if len(n.SourceKeys) > 0 {
				sb.WriteString(fmt.Sprintf("File: %s (use read_file to access)\n\n", n.SourceKeys[0]))
			}
		}
		return sb.String(), nil
	}
	// When SessionRoot is set, pass a SafePathValidator so relative paths
	// resolve against the workspace root instead of outputs/<taskID>.
	// When SessionRoot is empty, no validator is passed — backward compatible
	// with the legacy safeJoin behavior. (harness-bench report §4.2.)
	if h.SessionRoot != "" {
		taskDir := filepath.Join(h.OutputBase, taskID)
		validator := NewSafePathValidator(taskDir, h.SessionRoot, []string{h.SessionRoot})
		return HandleCoreTool(ctx, name, args, h.OutputBase, taskID, validator)
	}
	return HandleCoreTool(ctx, name, args, h.OutputBase, taskID)
}

func flattenChatMessages(msgs []ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteString("User: " + m.Content + "\n")
		case "assistant":
			b.WriteString("Assistant: " + m.Content + "\n")
		default:
			b.WriteString(m.Content + "\n")
		}
	}
	return b.String()
}

// toolCallSignature returns a deterministic string fingerprint of a set of
// tool calls. Used by the loop-guard to detect identical repeated calls.
func toolCallSignature(calls []schemas.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range calls {
		b.WriteString(c.Name)
		b.WriteByte('|')
		// Control args (`_reason`) are stripped from the payload, so they must
		// not distinguish two otherwise identical calls here either.
		data, _ := json.Marshal(canonicalToolArgs(c.Arguments))
		b.Write(data)
		b.WriteByte(';')
	}
	return b.String()
}

// artifactThreshold is the byte size above which ChatHarness tool results are
// externalized to a file and replaced with a structural skeleton + path reference.
// Below this, the result stays inline (no overhead). Above it, the content is
// written to a file and replaced with a FileSkeleton + path reference.
// Intentionally lower than AdaptiveOutputResult's 8KB because chat models have
// tighter context budgets (no task-level compression).
const artifactThreshold = 2 * 1024 // 2 KB

// readPassthroughCeiling is the size above which even a read_file result gets
// externalized. Reads are the model's deliberate recall, so they bypass the
// 2 KB externalization threshold — but not without bound, because read_file
// itself has no output cap and a 5 MB read would flood the window.
// 32 KB ≈ 8k tokens: small against the 200k windows these runs use, and a file
// larger than that can still be paged with read_file(start_line, end_line).
const readPassthroughCeiling = 32 * 1024

// shouldExternalize reports whether a tool result should be swapped for an
// artifact pointer + skeleton instead of being passed to the model verbatim.
//
// read_file is treated differently on purpose. It *is* the recall path: the
// pointer ends with "[Use read_file ... to read it.]". Replacing a read result
// with a skeleton therefore closes the loop on itself — the model asks for the
// content, receives a skeleton of field NAMES, and the only way to see the
// values is another read, which is externalized again.
//
// Measured on harness-bench 026 (v60, 2026-10-30): the delegated sub-agent wrote
// a 6 KB review_findings.json, the parent read it back and got
// `L14: "id": string / L15: "severity": string / ...`, then answered from that
// (oracle 0.35, failed_core 2). Before this, every tool result over 2 KB was
// invisible to the model no matter which tool produced it.
func (h *ChatHarness) shouldExternalize(toolName, result string) bool {
	if h.OutputBase == "" || len(result) <= artifactThreshold {
		return false
	}
	if isReadTool(toolName) {
		return len(result) > readPassthroughCeiling
	}
	return true
}

// sanitizeToolName replaces non-alphanumeric characters with underscores for
// safe use in filenames.
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// externalizeArtifact writes a large tool result to a file and returns a
// skeleton + path reference for in-context use. The full content remains
// accessible via read_file and is indexed in the ContextArchive for
// search_archive recall. On any error, returns the original result.
func (h *ChatHarness) externalizeArtifact(result, toolName, taskID string) string {
	if h.OutputBase == "" {
		return result
	}
	artDir := filepath.Join(h.OutputBase, taskID, "artifacts")
	if err := os.MkdirAll(artDir, 0755); err != nil {
		return result
	}
	filename := fmt.Sprintf("art_%d_%s.txt", time.Now().UnixNano(), sanitizeToolName(toolName))
	fullPath := filepath.Join(artDir, filename)
	if err := os.WriteFile(fullPath, []byte(result), 0644); err != nil {
		return result
	}
	absPath, _ := filepath.Abs(fullPath)

	skel := skeleton.DetectAndSkeletonize([]byte(result), toolName)

	// Never bloat. The full payload is already on disk, so the only thing the
	// skeleton has to do is cost less context than the original — that is the
	// entire point of externalizing. jsonSkeleton prints every key of every
	// array element, so a long array of objects can produce a "skeleton" that
	// is larger than the payload it replaces (observed: 3102 bytes of skeleton
	// for a 2521-byte JSON result). The other two summarizers in this package
	// are already covered by NeverBloat tests; this entry point was not.
	if len(skel) >= len(result) {
		if alt := skeleton.SummarizeOutput([]byte(result), toolName); len(alt) < len(result) {
			skel = alt
		} else {
			skel = fmt.Sprintf("[%d bytes, %d lines; skeleton omitted because it would not be smaller than the content]",
				len(result), strings.Count(result, "\n")+1)
		}
	}

	// Index in ContextArchive for search_archive recall.
	if h.Archive != nil {
		summary := skel
		if len(summary) > 500 {
			summary = summary[:500] + "\n... (skeleton truncated, see file)"
		}
		// audit fix L3: surface Archive.Append failure — previously swallowed,
		// so recall silently missed indexed artifacts with no signal.
		if aerr := h.Archive.Append(MemoryItem{
			TaskID:     taskID,
			NodeID:     fmt.Sprintf("artifact_%s_%d", taskID, time.Now().UnixNano()),
			Intent:     fmt.Sprintf("[artifact: %s]", toolName),
			Summary:    summary,
			SourceKeys: []string{absPath},
			Timestamp:  time.Now(),
		}); aerr != nil {
			fmt.Fprintf(os.Stderr, "[chat-harness] WARN: failed to index artifact in ContextArchive: %v\n", aerr)
		}
	}

	// The pointer must be actionable: read_file takes an absolute path (the
	// artifact dir is under TaskDir, which IsAllowed covers) and start_line /
	// end_line page through anything larger than the passthrough ceiling.
	return fmt.Sprintf(
		"[Artifact externalized: %d KB. Skeleton below. Full content at: %s]\n%s\n"+
			"[Use read_file on that path to read the full content; add start_line/end_line to page through a large file.]",
		len(result)/1024, absPath, skel,
	)
}
