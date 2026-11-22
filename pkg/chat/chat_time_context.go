package chat

import (
	"fmt"
	"os"
	"time"
)

// buildTimeContext renders the per-run time awareness block, or "" when
// disabled (VORTEX_CHAT_TIME_CONTEXT=0|off). Tail-stable by construction:
// appended to the last user message, never into the system prompt or the
// history body. Zero CreatedAt values cause their lines to be skipped —
// never fabricated.
func buildTimeContext(now time.Time, history []ChatMessage) string {
	if v := os.Getenv("VORTEX_CHAT_TIME_CONTEXT"); v == "0" || v == "off" {
		return ""
	}

	var b string
	header := "## Time context\n"
	nowLine := fmt.Sprintf("Now: %s (%s)\n", now.Format(time.RFC3339), weekdayEn(now))

	opened := firstCreatedAt(history)
	if !opened.IsZero() {
		openedLine := fmt.Sprintf("Session opened: %s\n", opened.Format(time.RFC3339))
		elapsedLine := fmt.Sprintf("Session elapsed: %s\n", now.Sub(opened).Round(time.Second).String())
		b = header + nowLine + openedLine + elapsedLine
	} else {
		b = header + nowLine
	}

	if gap, ok := prevUserGap(history); ok {
		b += fmt.Sprintf("Gap since previous user message: %s\n", gap.String())
	}

	return b
}

// firstCreatedAt returns the CreatedAt of the first message in history,
// or time.Time{} if history is empty.
func firstCreatedAt(history []ChatMessage) time.Time {
	if len(history) == 0 {
		return time.Time{}
	}
	return history[0].CreatedAt
}

// prevUserGap finds the last two user messages in history and returns the
// duration between their CreatedAt values (lastUser - prevUser). Returns
// (0, false) if fewer than 2 user messages exist or either CreatedAt is zero.
func prevUserGap(history []ChatMessage) (time.Duration, bool) {
	var lastIdx, prevIdx int = -1, -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			if lastIdx == -1 {
				lastIdx = i
			} else {
				prevIdx = i
				break
			}
		}
	}
	if lastIdx == -1 || prevIdx == -1 {
		return 0, false
	}
	last := history[lastIdx].CreatedAt
	prev := history[prevIdx].CreatedAt
	if last.IsZero() || prev.IsZero() {
		return 0, false
	}
	return last.Sub(prev), true
}

// appendTimeContext builds the time context block and appends it to the
// last user message in messages. No-op when the kill-switch is active or
// no user message exists. Computed once per Run — caller must not call
// this inside the agent loop.
func appendTimeContext(messages []ChatMessage, history []ChatMessage) {
	tc := buildTimeContext(time.Now(), history)
	if tc == "" {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			messages[i].Content += tc
			break
		}
	}
}

// weekdayEn returns the English weekday name for a time.Time.
func weekdayEn(t time.Time) string {
	return t.Weekday().String()
}
