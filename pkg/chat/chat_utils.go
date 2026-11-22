package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/schemas"
)

func flattenChatMessages(msgs []ChatMessage) string {
	threshold := timeMarksThreshold()
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 && threshold > 0 {
			if gap, ok := msgGap(msgs, i); ok && gap >= threshold {
				fmt.Fprintf(&b, "--- %s later ---\n", formatGap(gap))
			}
		}
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

// timeMarksThreshold reads VORTEX_CHAT_TIME_MARKS as a Go duration.
// Returns 0 (disabled) when unset, empty, or unparseable.
func timeMarksThreshold() time.Duration {
	v := os.Getenv("VORTEX_CHAT_TIME_MARKS")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// msgGap returns the duration between msgs[i].CreatedAt and msgs[i-1].CreatedAt.
// Returns (0, false) if either CreatedAt is zero.
func msgGap(msgs []ChatMessage, i int) (time.Duration, bool) {
	prev := msgs[i-1].CreatedAt
	cur := msgs[i].CreatedAt
	if prev.IsZero() || cur.IsZero() {
		return 0, false
	}
	return cur.Sub(prev), true
}

// formatGap renders a duration for the sparse time mark, e.g. "2h05m" or "10m".
func formatGap(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
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
