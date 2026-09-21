package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
}

func (h *ChatHarness) toolDefinitions() []schemas.ToolDefinition {
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
	b.WriteString("You are a concise, helpful assistant in a standalone chat harness.\n")
	b.WriteString("You have a small toolset: write_file, read_file, execute_code, delegate_to_orchestrator.\n")
	b.WriteString("Use tools when they help; answer directly otherwise. Do not call tools you do not need.\n")

	// ── Readiness Gate (Two-Tier Clarification Design, Module 1) ──────────────
	// Hard rule: the strong model (this harness) is the user-facing gateway. It
	// MUST perform Pre-codification Readiness Assessment before delegating to the
	// downstream weak-model orchestrator. Delegating an ambiguous task pollutes
	// the executor and burns retry budget on garbage inputs. See
	// docs/architecture/TWO_TIER_CLARIFICATION_AND_READINESS_DESIGN.md
	b.WriteString("\n## DELEGATION READINESS GATE (HARD RULE)\n")
	b.WriteString("Before calling delegate_to_orchestrator, you MUST verify the task is fully specified:\n")
	b.WriteString("1. All required parameters (paths, identifiers, formats) are concrete — no <TBD>/placeholder.\n")
	b.WriteString("2. No algorithmic ambiguity (e.g. 'sort it' without specifying key/order).\n")
	b.WriteString("3. No path or identifier contradictions across the request.\n")
	b.WriteString("4. The expected outcome / success criteria is explicit.\n")
	b.WriteString("If ANY of these fail, ask the user clarifying questions FIRST. NEVER blindly delegate a vague task.\n")

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
		}
	}
	return b.String()
}

// Run executes the agent loop and streams events via emit. It returns the
// final assistant text (for persistence) and any error.
func (h *ChatHarness) Run(ctx context.Context, taskID string, history []ChatMessage, emit func(ChatEvent) error) (string, error) {
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
	tools := h.toolDefinitions()
	mcpServers := []schemas.MCPServerDef{{Name: "core", Tools: tools}}

	messages := make([]ChatMessage, 0, len(history)+maxTurns)
	messages = append(messages, history...)

	var finalText strings.Builder // audit H1: was `var finalText string` with += (O(n²))
	var prevSig string
	toolFailCount := make(map[string]int)

	for turn := 0; turn < maxTurns; turn++ {
		// Abort if the context was cancelled (e.g. client disconnect or shutdown).
		if ctx.Err() != nil {
			emit(ChatEvent{Type: "error", Data: fmt.Sprintf("context cancelled: %v", ctx.Err())})
			return finalText.String(), ctx.Err()
		}

		req := providers.CompleteRequest{
			System:     system,
			User:       flattenChatMessages(messages),
			Model:      h.Model,
			MaxTokens:  2048,
			MCPServers: mcpServers,
		}

		resp, err := h.Provider.StreamComplete(ctx, req, func(chunk string) error {
			if chunk != "" {
				finalText.WriteString(chunk) // audit H1: was += (O(n²) string concat)
				if e := emit(ChatEvent{Type: "delta", Data: chunk}); e != nil {
					return e // audit H5: abort stream if client disconnected
				}
			}
			return nil
		})
		if err != nil {
			emit(ChatEvent{Type: "error", Data: err.Error()})
			return finalText.String(), err
		}

		if len(resp.ToolCalls) == 0 {
			emit(ChatEvent{Type: "done"})
			return finalText.String(), nil
		}

		// Loop-guard: detect repetitive tool calls and force a no-tools
		// completion to break the cycle. Uses the Sieve if available
		// (production-grade, threshold=3), else a lightweight signature check.
		shouldForce := false
		if h.Sieve != nil {
			interactions := make([]store.ToolInteraction, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				interactions[i] = store.ToolInteraction{ToolName: tc.Name, Arguments: tc.Arguments}
			}
			if ok, reason := h.Sieve.InspectToolCalls(taskID, interactions); !ok {
				emit(ChatEvent{Type: "loop_guard", Data: reason})
				shouldForce = true
			}
		} else {
			sig := toolCallSignature(resp.ToolCalls)
			if sig != "" && sig == prevSig {
				emit(ChatEvent{Type: "loop_guard", Data: "identical tool calls repeated; forcing final answer"})
				shouldForce = true
			}
			prevSig = sig
		}

		if shouldForce {
			noToolReq := req
			noToolReq.MCPServers = nil
			if r2, e2 := h.Provider.StreamComplete(ctx, noToolReq, func(chunk string) error {
				if chunk != "" {
					finalText.WriteString(chunk) // audit H1: was += (O(n²) string concat)
					if e := emit(ChatEvent{Type: "delta", Data: chunk}); e != nil {
						return e // audit H5: abort stream if client disconnected
					}
				}
				return nil
			}); e2 == nil && r2.Text != "" {
				emit(ChatEvent{Type: "done"})
				return finalText.String(), nil
			}
			emit(ChatEvent{Type: "done"})
			return finalText.String(), nil
		}

		for _, tc := range resp.ToolCalls {
			emit(ChatEvent{Type: "tool_call", Data: tc.Name, Meta: map[string]any{"args": tc.Arguments}})
			result, derr := h.execTool(ctx, tc.Name, tc.Arguments, taskID)
			if derr != nil {
				toolFailCount[tc.Name]++
				emit(ChatEvent{Type: "tool_error", Data: derr.Error(), Meta: map[string]any{"tool": tc.Name, "fail_count": toolFailCount[tc.Name]}})
				if toolFailCount[tc.Name] >= 3 {
					result = fmt.Sprintf("[TOOL %s HAS FAILED %d TIMES] Stop using this tool. Use an alternative approach or answer directly without tools.", tc.Name, toolFailCount[tc.Name])
				} else {
					result = fmt.Sprintf("[TOOL FAILED: %s] %s\nDo not abort. Either fix the arguments and retry, use a different tool, or answer directly.", tc.Name, derr.Error())
				}
			} else {
				toolFailCount[tc.Name] = 0
			}
			emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": tc.Name}})
			messages = append(messages, ChatMessage{Role: "tool", Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, result)})
		}
	}

	emit(ChatEvent{Type: "error", Data: fmt.Sprintf("max turns (%d) exceeded", maxTurns)})
	return finalText.String(), fmt.Errorf("max turns (%d) exceeded", maxTurns)
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
