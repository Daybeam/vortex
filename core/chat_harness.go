package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ChatMessage is a single turn in a chat session. ID and ParentID form a
// tree: branching from a past message creates a new node with ParentID set
// to the replied-to message's ID. Empty ParentID means root.
type ChatMessage struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Role      string    `json:"role"` // "user" | "assistant" | "tool"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at,omitempty"`
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
	MaxTurns int
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
	// See docs/STEP_PLAN_MODE_DESIGN.md §10.3.
	PendingSteer string
	SteerLock    sync.Mutex

	// ReadinessGateSoft (ADDED 2026-10-10): when true, softens the
	// delegation readiness gate language from "NEVER blindly delegate"
	// to a suggestion. Weak models (≤7B) over-apply the hard rule and
	// ask excessive clarifying questions instead of acting. Default
	// false = strict (current behavior for strong models).
	// See eval-kit/docs/28-engine-path-analysis.md §2 claim ②.
	ReadinessGateSoft bool

	// Segments (ADDED 2026-10-10): optional topic segment manager for
	// chat memory. When non-nil, each indexed turn is also processed by
	// the SegmentManager for topic segmentation (STAY/SEAL decisions).
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
	Segments *SegmentManager
}

func (h *ChatHarness) ToolDefinitions() []schemas.ToolDefinition {
	tools := CoreToolDefinitions()
	tools = append(tools, schemas.ToolDefinition{
		Name: "delegate_to_orchestrator",
		Description: "Delegate a complex, multi-step task to the orchestrator (role-based subagent execution). " +
			"Use only for work needing planning, multiple tools, or specialized roles. " +
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

	var finalText strings.Builder // audit H1: was `var finalText string` with += (O(n²))
	var prevSig string
	toolFailErrors := make(map[string][]string) // per-tool recent error messages (capped at 5)
	noTextTurns := 0                            // consecutive turns with tool calls but no text output

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
			MaxTokens:   2048,
			MCPServers:  mcpServers,
			Attachments: h.Attachments,
		}

		// Stream chunk detector (per-turn, per-call): catches intra-call
		// repetitive output that tool-call-centric guards cannot see — e.g.
		// a weak model emitting dozens of identical reasoning/text chunks
		// without any tool calls. Inspired by Cordis thinking-loop-guard.
		chunkDetector := NewStreamChunkDetector()
		resp, err := h.Provider.StreamComplete(ctx, req, func(chunk string) error {
			if chunk != "" {
				if e := chunkDetector.Check(chunk); e != nil {
					return e // abort stream; handled below as steer, not fatal
				}
				finalText.WriteString(chunk) // audit H1: was += (O(n²) string concat)
				if e := emit(ChatEvent{Type: "delta", Data: chunk}); e != nil {
					return e // audit H5: abort stream if client disconnected
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrRepetitiveOutput) {
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

		// Loop detection: log repetitive tool calls for observability.
		// Consequence is cache replay (in the execution loop below), not
		// force-termination — force-terminate causes small models to panic
		// (empty turns / transfer-to-human) instead of continuing.
		if h.Sieve != nil {
			interactions := make([]store.ToolInteraction, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				interactions[i] = store.ToolInteraction{ToolName: tc.Name, Arguments: tc.Arguments}
			}
			if ok, reason := h.Sieve.InspectToolCalls(taskID, interactions); !ok {
				emit(ChatEvent{Type: "loop_guard", Data: reason})
			}
		} else {
			sig := toolCallSignature(resp.ToolCalls)
			if sig != "" && sig == prevSig {
				emit(ChatEvent{Type: "loop_guard", Data: "identical tool calls repeated"})
			}
			prevSig = sig
		}

		for i, tc := range resp.ToolCalls {
			emit(ChatEvent{Type: "tool_call", Data: tc.Name, Meta: map[string]any{"args": tc.Arguments}})

			// Plan drift detection (#4 + #10, ADDED 2026-10-10):
			// Check the first tool call in the batch against the step
			// plan. If drift is detected, set PendingSteer — it will be
			// appended to the last tool result below.
			// See docs/STEP_PLAN_MODE_DESIGN.md §10.3.
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
			}

			// Append PendingSteer to the last tool result in this batch.
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
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.1.
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
		return "Delegated to orchestrator task " + id, nil
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
		data, _ := json.Marshal(c.Arguments)
		b.Write(data)
		b.WriteByte(';')
	}
	return b.String()
}