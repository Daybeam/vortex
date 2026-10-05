package core

import (
	"context"
	"strings"
	"testing"
)

func TestRollingWindowMemory_ShortHistoryPassthrough(t *testing.T) {
	m := NewRollingWindowMemory(10)
	history := []ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}

	result := m.Process(context.Background(), "s1", history)
	if len(result) != len(history) {
		t.Fatalf("expected %d messages (passthrough), got %d", len(history), len(result))
	}
	for i, msg := range result {
		if msg.Content != history[i].Content {
			t.Fatalf("message %d content mismatch: got %q, want %q", i, msg.Content, history[i].Content)
		}
	}
}

func TestRollingWindowMemory_LongHistoryWindowed(t *testing.T) {
	m := NewRollingWindowMemory(4)
	history := make([]ChatMessage, 20)
	for i := 0; i < 10; i++ {
		history[i*2] = ChatMessage{Role: "user", Content: "question " + string(rune('A'+i))}
		history[i*2+1] = ChatMessage{Role: "assistant", Content: "answer " + string(rune('A'+i))}
	}

	result := m.Process(context.Background(), "s1", history)

	if len(result) != 5 {
		t.Fatalf("expected 5 messages (1 summary + 4 recent), got %d", len(result))
	}

	if result[0].Role != "system" {
		t.Fatalf("expected first message role 'system' (summary), got %q", result[0].Role)
	}
	if !strings.Contains(result[0].Content, "Conversation Summary") {
		t.Fatal("summary message should contain 'Conversation Summary' header")
	}

	for i := 1; i < len(result); i++ {
		if result[i].Role != history[len(history)-4+(i-1)].Role {
			t.Fatalf("message %d role mismatch", i)
		}
		if result[i].Content != history[len(history)-4+(i-1)].Content {
			t.Fatalf("message %d content mismatch: got %q, want %q",
				i, result[i].Content, history[len(history)-4+(i-1)].Content)
		}
	}
}

func TestRollingWindowMemory_SummaryContainsOlderContent(t *testing.T) {
	m := NewRollingWindowMemory(2)
	history := []ChatMessage{
		{Role: "user", Content: "What is Go?"},
		{Role: "assistant", Content: "Go is a programming language."},
		{Role: "user", Content: "How do I install it?"},
		{Role: "assistant", Content: "Download from golang.org."},
		{Role: "user", Content: "Thanks!"},
		{Role: "assistant", Content: "You're welcome!"},
	}

	result := m.Process(context.Background(), "s1", history)

	if result[0].Role != "system" {
		t.Fatal("first message should be the summary")
	}
	summary := result[0].Content
	if !strings.Contains(summary, "What is Go?") {
		t.Fatal("summary should contain older user message 'What is Go?'")
	}
	if !strings.Contains(summary, "programming language") {
		t.Fatal("summary should contain older assistant response")
	}
}

func TestRollingWindowMemory_DefaultWindowSize(t *testing.T) {
	m := NewRollingWindowMemory(0)
	if m.WindowSize != 10 {
		t.Fatalf("expected default window size 10, got %d", m.WindowSize)
	}
	m2 := NewRollingWindowMemory(-5)
	if m2.WindowSize != 10 {
		t.Fatalf("expected default window size 10 for negative input, got %d", m2.WindowSize)
	}
}

func TestChatHarness_NilMemoryPassthrough(t *testing.T) {
	h := &ChatHarness{}
	if h.Memory != nil {
		t.Fatal("Memory should be nil by default")
	}
}

func TestRollingWindowMemory_ExactWindowSizePassthrough(t *testing.T) {
	m := NewRollingWindowMemory(4)
	history := []ChatMessage{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
		{Role: "assistant", Content: "d"},
	}

	result := m.Process(context.Background(), "s1", history)
	if len(result) != 4 {
		t.Fatalf("expected 4 messages (exact window = passthrough), got %d", len(result))
	}
}
