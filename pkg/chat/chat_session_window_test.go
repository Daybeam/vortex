package chat

import (
	"testing"
)

func TestSnapshotMessagesWindow_NoLimit(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AppendUserMessage("u1", "msg1", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply1"}, "")
	s.AppendUserMessage("u2", "msg2", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply2"}, "")

	msgs, hasMore, oldestID := s.SnapshotMessagesWindow(0, "")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if hasMore {
		t.Fatal("expected hasMore=false for full path with no limit")
	}
	if oldestID != msgs[0].ID {
		t.Fatalf("oldestID should be first message, got %q vs %q", oldestID, msgs[0].ID)
	}
	if msgs[0].Content != "msg1" || msgs[3].Content != "reply2" {
		t.Fatalf("expected chronological order msg1..reply2, got %q..%q", msgs[0].Content, msgs[3].Content)
	}
}

func TestSnapshotMessagesWindow_WithLimit(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	for i := 0; i < 10; i++ {
		s.AppendUserMessage("", "msg", "", "")
		s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	// 20 messages, pages of 4 = 5 full pages
	cursor := ""
	for page := 1; page <= 5; page++ {
		msgs, hasMore, oldestID := s.SnapshotMessagesWindow(4, cursor)
		if len(msgs) != 4 {
			t.Fatalf("page %d: expected 4 messages, got %d", page, len(msgs))
		}
		if oldestID != msgs[0].ID {
			t.Fatalf("page %d: oldestID mismatch", page)
		}
		if page < 5 && !hasMore {
			t.Fatalf("page %d: expected hasMore=true", page)
		}
		if page == 5 && hasMore {
			t.Fatal("page 5: expected hasMore=false (reached root)")
		}
		cursor = oldestID
	}

	// Beyond root: empty
	msgs, hasMore, _ := s.SnapshotMessagesWindow(4, cursor)
	if len(msgs) != 0 || hasMore {
		t.Fatal("expected empty result beyond root")
	}
}

func TestSnapshotMessagesWindow_LimitExactlyReachesRoot(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AppendUserMessage("u1", "msg1", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply1"}, "")
	s.AppendUserMessage("u2", "msg2", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply2"}, "")

	msgs, hasMore, _ := s.SnapshotMessagesWindow(4, "")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if hasMore {
		t.Fatal("expected hasMore=false when limit exactly reaches root")
	}
}

func TestSnapshotMessagesWindow_BeforeIsRoot(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AppendUserMessage("u1", "msg1", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply1"}, "")

	path := s.SnapshotMessages()
	rootID := path[0].ID

	msgs, hasMore, _ := s.SnapshotMessagesWindow(10, rootID)
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages when before=root, got %d", len(msgs))
	}
	if hasMore {
		t.Fatal("expected hasMore=false when before is root")
	}
}

func TestSnapshotMessagesWindow_BeforeNotFound(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AppendUserMessage("u1", "msg1", "", "")

	msgs, hasMore, _ := s.SnapshotMessagesWindow(10, "nonexistent")
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages for unknown before, got %d", len(msgs))
	}
	if hasMore {
		t.Fatal("expected hasMore=false for unknown before")
	}
}

func TestSnapshotMessagesWindow_EmptySession(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}

	msgs, hasMore, oldestID := s.SnapshotMessagesWindow(10, "")
	if msgs != nil {
		t.Fatalf("expected nil for empty session, got %v", msgs)
	}
	if hasMore {
		t.Fatal("expected hasMore=false for empty session")
	}
	if oldestID != "" {
		t.Fatalf("expected empty oldestID for empty session, got %q", oldestID)
	}
}

func TestSnapshotMessagesWindow_SingleMessage(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AppendUserMessage("u1", "only", "", "")

	msgs, hasMore, oldestID := s.SnapshotMessagesWindow(10, "")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if hasMore {
		t.Fatal("expected hasMore=false for single message (it's root)")
	}
	if oldestID != msgs[0].ID {
		t.Fatalf("oldestID mismatch")
	}
}

func TestSnapshotMessagesWindow_BranchSwitch(t *testing.T) {
	s := &ChatSession{ID: "s1", Messages: make(map[string]*ChatMessage), seenMsg: map[string]bool{}}
	s.AppendUserMessage("u1", "msg1", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply1"}, "")
	s.AppendUserMessage("u2", "msg2", "", "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "reply2"}, "")

	path := s.SnapshotMessages()
	branchPoint := path[1].ID

	s.AppendUserMessage("u3", "forked", branchPoint, "")
	s.AppendMessage(ChatMessage{Role: "assistant", Content: "forked-reply"}, "")

	msgs, hasMore, _ := s.SnapshotMessagesWindow(2, "")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages on new branch, got %d", len(msgs))
	}
	if msgs[0].Content != "forked" || msgs[1].Content != "forked-reply" {
		t.Fatalf("expected forked branch messages, got %q, %q", msgs[0].Content, msgs[1].Content)
	}
	if !hasMore {
		t.Fatal("expected hasMore=true (branch point has a parent)")
	}

	msgs2, hasMore2, _ := s.SnapshotMessagesWindow(2, msgs[0].ID)
	if len(msgs2) != 2 {
		t.Fatalf("expected 2 older messages, got %d", len(msgs2))
	}
	if msgs2[0].Content != "msg1" || msgs2[1].Content != "reply1" {
		t.Fatalf("expected original branch start, got %q, %q", msgs2[0].Content, msgs2[1].Content)
	}
	if hasMore2 {
		t.Fatal("expected hasMore=false (msg1 is root, no older messages)")
	}
}
