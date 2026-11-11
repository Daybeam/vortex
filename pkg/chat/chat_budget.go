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
// a real failure. Callers that keep only max-turns partials use this; new code
// should prefer IsPartialRun, which also keeps the exploration ceiling.
func IsMaxTurnsPartial(err error) bool {
	return errors.Is(err, ErrMaxTurnsExceeded)
}

// IsPartialRun reports whether a Run error is one of the deliberate ceilings —
// the turn budget or the opt-in exploration ceiling — rather than a real
// failure. Callers use it to keep the partial text; the two ceilings share that
// contract but have different causes and different markers.
func IsPartialRun(err error) bool {
	return errors.Is(err, ErrMaxTurnsExceeded) || errors.Is(err, ErrExplorationExhausted)
}

// ErrExplorationExhausted is the sentinel the opt-in escalate arm returns when
// a run keeps exploring without writing a file past the last threshold. It is
// not a failure: the partial text is the only evidence the run produced and
// must be kept exactly like a max-turns wreck.
var ErrExplorationExhausted = errors.New("chat exploration budget exhausted without a write")

// ExplorationTruncatedPrefix labels an exploration-ceiling partial. The reason
// is part of the text because the follow-up turn reads only this string.
const ExplorationTruncatedPrefix = "[PARTIAL — exploration ceiling reached"

// markExplorationTruncated labels an exploration-ceiling partial. An empty
// wreck stays empty for the same reason as markTruncated.
func markExplorationTruncated(text string, calls int) string {
	if text == "" {
		return text
	}
	return fmt.Sprintf("%s: %d calls without a write] %s", ExplorationTruncatedPrefix, calls, text)
}

// IsTruncated reports whether a final text is a marked partial. Retrieval
// indexes must refuse these (Q5-3: a half answer embedded as knowledge
// outranks the complete answer that would have followed); callers should gate
// on err == nil first and use this as the second, content-level veto.
func IsTruncated(text string) bool {
	return strings.HasPrefix(text, TruncatedMarkerPrefix)
}
