package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestChatHarness_ZeroMaxTurnsUsesDefault pins the loop's own fallback: a
// harness built without MaxTurns (any caller that bypasses
// pkg/api/chat_handlers.go) must get the 2026-11-04 budget, not the old 6.
//
// Regression the operator raised on 2026-11-04: 6 turns ended multi-step chat
// work mid-task, and the chat loop cannot be resumed the way the DAG path can.
func TestChatHarness_ZeroMaxTurnsUsesDefault(t *testing.T) {
	h := &ChatHarness{
		Provider:   &writeOnlyProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		// MaxTurns deliberately left at its zero value.
	}

	var events []ChatEvent
	_, err := h.Run(context.Background(), "task_budget_default",
		[]ChatMessage{{Role: "user", Content: "keep writing"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	want := fmt.Sprintf("max turns (%d) exceeded", DefaultMaxTurns)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected the loop to stop at %s, got err=%v", want, err)
	}
	for _, ev := range events {
		if ev.Type == "error" && strings.Contains(ev.Data, want) {
			return
		}
	}
	t.Errorf("no error event carried %q; last events: %v", want, events[len(events)-3:])
}
