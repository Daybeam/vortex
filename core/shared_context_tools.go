package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// SharedContextToolNames identifies the shared-context built-in tools.
// These are NOT in CoreToolNames — they are handled separately because
// they require a *store.SharedContext instance from the SpawnRequest.
var SharedContextToolNames = map[string]bool{
	"shared_context_write": true,
	"shared_context_read":  true,
}

// SharedContextToolDefinitions returns the MCP tool schemas for
// shared_context_write and shared_context_read.
func SharedContextToolDefinitions() []schemas.ToolDefinition {
	return []schemas.ToolDefinition{
		{
			Name:        "shared_context_write",
			Description: "Write a typed knowledge entry to the shared context for other concurrent agents to read. Use FAIL when you discover a dead end, FACT for confirmed truths, OBSERVED for lightweight notes, CLAIM to signal scope ownership, PATCH_SUMMARY to describe a change you made.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"FAIL", "FACT", "OBSERVED", "CLAIM", "PATCH_SUMMARY"},
						"description": "Entry type. FAIL=dead end, FACT=confirmed truth, OBSERVED=observation, CLAIM=scope claim, PATCH_SUMMARY=change summary",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Short summary (100-300 chars). Must be self-contained.",
					},
					"detail": map[string]any{
						"type":        "string",
						"description": "Optional: supporting evidence (tool output, code diff, log excerpt)",
					},
				},
				"required": []string{"type", "content"},
			},
		},
		{
			Name:        "shared_context_read",
			Description: "Read recent typed knowledge entries from the shared context. Returns entries from other agents working on the same task — use this to avoid repeating known failures or to build on established facts.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type_filter": map[string]any{
						"type":        "string",
						"description": "Optional: filter by entry type (FAIL, FACT, OBSERVED, CLAIM, PATCH_SUMMARY)",
					},
					"limit": map[string]any{
						"type":        "integer",
						"default":     10,
						"description": "Max entries to return (most recent first)",
					},
				},
			},
		},
	}
}

// handleSharedContextTool executes a shared_context tool call. The author
// field is auto-filled from the step ID — the agent cannot forge another
// agent's identity.
func handleSharedContextTool(call schemas.ToolCall, sc *store.SharedContext, taskID, stepID string) (string, string) {
	switch call.Name {
	case "shared_context_write":
		return handleSharedContextWrite(call, sc, taskID, stepID)
	case "shared_context_read":
		return handleSharedContextRead(call, sc, taskID)
	default:
		return fmt.Sprintf("Error: unknown shared_context tool: %s", call.Name), "error"
	}
}

func handleSharedContextWrite(call schemas.ToolCall, sc *store.SharedContext, taskID, stepID string) (string, string) {
	entryType, _ := call.Arguments["type"].(string)
	content, _ := call.Arguments["content"].(string)
	detail, _ := call.Arguments["detail"].(string)

	if content == "" {
		return "Error: content is required", "error"
	}

	validTypes := map[string]bool{
		"FAIL": true, "FACT": true, "OBSERVED": true, "CLAIM": true, "PATCH_SUMMARY": true,
	}
	if !validTypes[entryType] {
		return fmt.Sprintf("Error: invalid type %q; must be one of FAIL, FACT, OBSERVED, CLAIM, PATCH_SUMMARY", entryType), "error"
	}

	entry := &store.SharedContextEntry{
		Type:    store.ContextEntryType(entryType),
		Content: content,
		Detail:  detail,
		Author:  stepID,
		StepID:  stepID,
	}

	written, err := sc.WriteEntry(entry)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), "error"
	}

	return fmt.Sprintf("Written entry %s (type=%s) to shared context for task %s.", written.ID, entryType, taskID), "success"
}

func handleSharedContextRead(call schemas.ToolCall, sc *store.SharedContext, taskID string) (string, string) {
	typeFilter, _ := call.Arguments["type_filter"].(string)
	limit := 10
	if l, ok := call.Arguments["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}

	entries, err := sc.ReadRecent(limit, store.ContextEntryType(typeFilter))
	if err != nil {
		return fmt.Sprintf("Error: %v", err), "error"
	}

	if len(entries) == 0 {
		return "No shared context entries found.", "success"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Shared context for task %s (%d entries):\n\n", taskID, len(entries)))
	for i, e := range entries {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s: %s\n", i+1, e.Type, e.Author, e.Content))
		if e.Detail != "" {
			// Truncate detail to 200 chars for readability.
			d := e.Detail
			if len(d) > 200 {
				d = d[:200] + "..."
			}
			sb.WriteString(fmt.Sprintf("   Detail: %s\n", d))
		}
		sb.WriteString("\n")
	}
	return sb.String(), "success"
}

// sharedContextServerDef builds the MCP server definition for shared_context
// tools. Returns nil if sc is nil (no shared context for this spawn).
func sharedContextServerDef() schemas.MCPServerDef {
	return schemas.MCPServerDef{
		Name:  "_shared_context",
		URL:   "sharedcontext://internal",
		Tools: SharedContextToolDefinitions(),
	}
}

// marshalSharedContextResult serializes entries for tool return.
func marshalSharedContextResult(entries []*store.SharedContextEntry) string {
	data, err := json.Marshal(entries)
	if err != nil {
		return "[]"
	}
	return string(data)
}
