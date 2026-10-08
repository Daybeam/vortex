package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRollingWindowMemory_ShortHistoryUnchanged(t *testing.T) {
	mem := NewRollingWindowMemory(10)
	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "Hello"},
		{ID: "2", Role: "assistant", Content: "Hi there!"},
	}

	got := mem.Process(context.Background(), "session1", history)
	if len(got) != 2 {
		t.Fatalf("Expected 2 messages for short history, got %d", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "2" {
		t.Errorf("Short history messages altered")
	}
}

func TestRollingWindowMemory_CompressesOlderHistory(t *testing.T) {
	mem := NewRollingWindowMemory(4)
	history := make([]ChatMessage, 12)
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history[i] = ChatMessage{
			ID:      fmt.Sprintf("msg_%d", i),
			Role:    role,
			Content: fmt.Sprintf("Turn %d message content", i),
		}
	}

	got := mem.Process(context.Background(), "session123", history)

	// Expected: 1 summary message + 4 recent messages = 5 total
	if len(got) != 5 {
		t.Fatalf("Expected 5 messages after compression, got %d", len(got))
	}

	if got[0].Role != "system" {
		t.Errorf("First message should be system summary, got %s", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "Prior Conversation Summary:") {
		t.Errorf("Summary header missing in first message")
	}

	// Recent 4 messages should match history[8..11]
	for i := 0; i < 4; i++ {
		if got[i+1].ID != fmt.Sprintf("msg_%d", i+8) {
			t.Errorf("Expected recent msg_%d at index %d, got %s", i+8, i+1, got[i+1].ID)
		}
	}
}
