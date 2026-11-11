package chat

import (
	"encoding/json"
	"strings"

	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/schemas"
)

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
		data, _ := json.Marshal(sieve.CanonicalToolArgs(c.Arguments))
		b.Write(data)
		b.WriteByte(';')
	}
	return b.String()
}
