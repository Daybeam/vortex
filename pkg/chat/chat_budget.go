package chat

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultMaxTurns is the chat agent-loop budget when nothing configures it.
//
// 50 as of 2026-11-04. The previous 6 ended turns in the middle of multi-step
// work, and unlike the DAG path the chat loop has no way to pick a run back up:
// running out returns "max turns (N) exceeded" (chat_harness.go:1132-1133) and
// the task is over. A larger ceiling is the only mitigation until resume exists.
//
// Both entry points use it — the loop's own zero-value fallback
// (chat_harness.go:491-493) and pkg/api/chat_handlers.go, where config
// chat_max_turns / max_tool_turns can override it.
const DefaultMaxTurns = 50

// ErrMaxTurnsExceeded is the sentinel the loop returns when it reaches the
// ceiling without a final answer. Before Q5-3 the caller could only string-
// match "max turns (%d) exceeded" — which is how the two API entry points ended
// up treating the wreck like any other provider failure and discarding it, even
// though the text accumulated over N turns is worth more to the user than
// nothing. Callers that want to keep it must test this sentinel, not the string.
var ErrMaxTurnsExceeded = errors.New("chat turn budget exhausted")

// TruncatedMarkerPrefix is prepended to whatever partial text a ceiling-exhausted
// run produced. The marker is part of the returned string, not a side channel:
// it survives session persistence, so a transcript read days later still says
// "this answer was cut off" instead of reading like a complete one.
const TruncatedMarkerPrefix = "[PARTIAL — turn ceiling reached"

// markTruncated labels a ceiling-exhausted partial. An empty wreck stays empty —
// there is nothing to qualify, and a lone marker would look like an answer.
func markTruncated(text string, maxTurns int) string {
	if text == "" {
		return text
	}
	return fmt.Sprintf("%s: %d turns used] %s", TruncatedMarkerPrefix, maxTurns, text)
}

// IsMaxTurnsPartial reports whether a Run error is the turn ceiling rather than
// a real failure. This is the only gate a caller needs to keep the partial.
func IsMaxTurnsPartial(err error) bool {
	return errors.Is(err, ErrMaxTurnsExceeded)
}

// IsTruncated reports whether a final text is a marked partial. Retrieval
// indexes must refuse these (Q5-3: a half answer embedded as knowledge
// outranks the complete answer that would have followed); callers should gate
// on err == nil first and use this as the second, content-level veto.
func IsTruncated(text string) bool {
	return strings.HasPrefix(text, TruncatedMarkerPrefix)
}
