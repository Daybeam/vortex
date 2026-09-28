package core

import (
	"testing"
	"time"
)

func TestChatSessionClear_ResetsAllState(t *testing.T) {
	s := &ChatSession{
		ID:        "test-session",
		Messages:  make(map[string]*ChatMessage),
		CreatedAt: time.Now(),
		seenMsg:   make(map[string]bool),
	}

	// Populate with messages and events.
	s.AppendUserMessage("msg-1", "hello", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "hi"}, "")
	s.AddEvent(ChatEvent{Type: "done"})
	s.AddEvent(ChatEvent{Type: "done"})

	if len(s.Messages) != 2 {
		t.Fatalf("expected 2 messages before clear, got %d", len(s.Messages))
	}
	if s.RootID == "" {
		t.Fatal("expected RootID to be set before clear")
	}
	if s.ActiveLeafID == "" {
		t.Fatal("expected ActiveLeafID to be set before clear")
	}
	preClearSeq := s.nextSeq
	if preClearSeq == 0 {
		t.Fatal("expected nextSeq > 0 before clear")
	}

	// Clear and verify.
	s.Clear()

	if len(s.Messages) != 0 {
		t.Errorf("expected 0 messages after clear, got %d", len(s.Messages))
	}
	if s.RootID != "" {
		t.Errorf("expected RootID empty after clear, got %q", s.RootID)
	}
	if s.ActiveLeafID != "" {
		t.Errorf("expected ActiveLeafID empty after clear, got %q", s.ActiveLeafID)
	}
	if len(s.events) != 0 {
		t.Errorf("expected 0 events after clear, got %d", len(s.events))
	}
	if len(s.seenMsg) != 0 {
		t.Errorf("expected 0 seenMsg after clear, got %d", len(s.seenMsg))
	}
	// nextSeq must be preserved so SSE Last-Event-ID resume keeps working.
	if s.nextSeq != preClearSeq {
		t.Errorf("expected nextSeq preserved (%d), got %d", preClearSeq, s.nextSeq)
	}
}

func TestChatSessionClear_NewEventsHaveHigherSeq(t *testing.T) {
	s := &ChatSession{
		Messages: make(map[string]*ChatMessage),
		seenMsg:  make(map[string]bool),
	}
	s.AddEvent(ChatEvent{Type: "done"})
	oldSeq := s.nextSeq

	s.Clear()
	s.AddEvent(ChatEvent{Type: "done"})

	if s.events[0].Seq <= oldSeq {
		t.Errorf("after clear, new event Seq %d should be > pre-clear nextSeq %d",
			s.events[0].Seq, oldSeq)
	}
}
