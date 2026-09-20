package core

import (
	"path/filepath"
	"testing"
)

func TestChatSessionEventReplay(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AddEvent(ChatEvent{Type: "delta", Data: "a"})
	s.AddEvent(ChatEvent{Type: "delta", Data: "b"})
	s.AddEvent(ChatEvent{Type: "done"})

	if got := s.EventsAfter(0); len(got) != 3 {
		t.Fatalf("expected 3 events from seq 0, got %d", len(got))
	}
	if got := s.EventsAfter(1); len(got) != 2 || got[0].Data != "b" {
		t.Fatalf("expected 2 events after seq 1 starting with 'b', got %v", got)
	}
	if got := s.EventsAfter(3); len(got) != 0 {
		t.Fatalf("expected 0 events after last seq, got %d", len(got))
	}
}

func TestChatSessionAppendUserMessageIdempotent(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	if !s.AppendUserMessage("m1", "hello", "") {
		t.Fatal("first append should be accepted")
	}
	if s.AppendUserMessage("m1", "hello", "") {
		t.Fatal("duplicate messageID should be rejected")
	}
	if s.AppendUserMessage("m2", "world", "") {
		if len(s.SnapshotMessages()) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(s.SnapshotMessages()))
		}
	}
}

func TestChatSessionStorePersistAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat")
	st := NewChatSessionStore(dir, nil)

	s := st.GetOrCreate("abc")
	s.AppendUserMessage("m1", "first", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply"}, "")

	if err := st.Persist(s); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	// Fresh store reading the same dir.
	st2 := NewChatSessionStore(dir, nil)
	s2 := st2.GetOrCreate("abc")
	msgs := s2.SnapshotMessages()
	if len(msgs) != 2 || msgs[0].Content != "first" || msgs[1].Content != "reply" {
		t.Fatalf("loaded messages mismatch: %v", msgs)
	}
}

func TestChatSessionBranching(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}

	// Build a linear conversation: user1 → assistant1 → user2 → assistant2
	s.AppendUserMessage("u1", "what is 1+1", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "2"}, "")
	s.AppendUserMessage("u2", "what is 2+2", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "4"}, "")

	// Active path should be 4 messages long.
	path := s.SnapshotMessages()
	if len(path) != 4 {
		t.Fatalf("expected 4 messages in active path, got %d", len(path))
	}

	// Branch from the first assistant reply (path[1]).
	branchPoint := path[1].ID
	s.AppendUserMessage("u3", "actually, what is 1+2?", branchPoint)
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "3"}, "")

	// Active path should now be 3 messages: user1 → assistant1 → user3 → assistant3
	path = s.SnapshotMessages()
	if len(path) != 4 {
		t.Fatalf("expected 4 messages in branched path, got %d", len(path))
	}
	if path[2].Content != "actually, what is 1+2?" {
		t.Fatalf("expected branched user message at index 2, got %q", path[2].Content)
	}
	if path[3].Content != "3" {
		t.Fatalf("expected branched assistant reply at index 3, got %q", path[3].Content)
	}

	// The old branch (user2 → assistant2) should still exist in the map.
	totalMsgs := 0
	for range s.Messages {
		totalMsgs++
	}
	// 4 original + 2 branched = 6 total nodes in the tree.
	if totalMsgs != 6 {
		t.Fatalf("expected 6 total messages in tree, got %d", totalMsgs)
	}

	// ListBranches should show 2 children at the branch point.
	children := s.ListBranches(branchPoint)
	if len(children) != 2 {
		t.Fatalf("expected 2 branches at branch point, got %d", len(children))
	}
}
