package core

import (
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/store"
)

// Regression test for the client-id divergence bug: the web UI chains later
// turns onto its own optimistic message ids (msg_<ts>_...), while the backend
// stores each message under a fresh uuid. Before the fix, parent_id /
// reply_to were persisted verbatim, so every message after the second had a
// dangling parent and the active path collapsed to the last message on
// reload. AppendUserMessage must resolve client ids to stored ids, and the
// mapping must survive persist + reload (client_id column / json field).
func TestAppendUserMessage_ResolvesClientIDsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "chat.db")
	db, err := store.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	backend := store.NewSQLiteChatBackend(db)

	st := NewChatSessionStore(dir, backend)
	s := st.GetOrCreate("sess-1")

	// ── Live flow, mirroring the web client ───────────────────────────
	// 1. First message: no parent (root). Client id optA.
	if !s.AppendUserMessage("optA", "A", "", "") {
		t.Fatal("append A rejected")
	}
	storedA := s.ActiveLeafID
	if storedA == "" || storedA == "optA" {
		t.Fatalf("stored id should be a backend id, got %q", storedA)
	}

	// 2. Second message chained onto optA (client id of the root).
	if !s.AppendUserMessage("optB", "B", "optA", "") {
		t.Fatal("append B rejected")
	}
	// 3. Branch back to the root: parent optA is NOT the active leaf (B) → fork.
	if !s.AppendUserMessage("optC", "C", "optA", "") {
		t.Fatal("append C rejected")
	}
	// 4. Reply to the root while chaining onto the branch message (optC).
	if !s.AppendUserMessage("optD", "D", "optC", "optA") {
		t.Fatal("append D rejected")
	}

	// ── Assertions on the live tree ───────────────────────────────────
	assertTree := func(t *testing.T, s *ChatSession, context string) {
		t.Helper()
		// The tree keeps all four messages; B branched off A and left the
		// active path (that's what "branch" means), so the path is A→C→D.
		if len(s.Messages) != 4 {
			t.Fatalf("%s: tree has %d messages, want 4", context, len(s.Messages))
		}
		msgs := s.SnapshotMessages()
		if len(msgs) != 3 {
			t.Fatalf("%s: active path has %d messages, want 3 (A, C, D)", context, len(msgs))
		}
		path := map[string]*ChatMessage{}
		for i := range msgs {
			path[msgs[i].Content] = &msgs[i]
		}
		// No dangling parents anywhere in the tree.
		for _, m := range s.Messages {
			if m.ParentID != "" {
				if _, ok := s.Messages[m.ParentID]; !ok {
					t.Errorf("%s: message %q has dangling parent %q", context, m.Content, m.ParentID)
				}
			}
		}
		// B (off-path) still hangs off A's stored id.
		var b *ChatMessage
		for _, m := range s.Messages {
			if m.Content == "B" {
				b = m
			}
		}
		if b == nil {
			t.Fatalf("%s: B missing from tree", context)
		}
		if b.ParentID != path["A"].ID {
			t.Errorf("%s: B parent = %q, want A's stored id %q", context, b.ParentID, path["A"].ID)
		}
		if p := path["C"].ParentID; p != path["A"].ID {
			t.Errorf("%s: C parent = %q, want A's stored id %q", context, p, path["A"].ID)
		}
		if f := path["C"].ForkedFrom; f != path["A"].ID {
			t.Errorf("%s: C forked_from = %q, want A's stored id %q", context, f, path["A"].ID)
		}
		if p := path["D"].ParentID; p != path["C"].ID {
			t.Errorf("%s: D parent = %q, want C's stored id %q", context, p, path["C"].ID)
		}
		if f := path["D"].ForkedFrom; f != "" {
			t.Errorf("%s: D should NOT be a fork (parent == leaf), got forked_from=%q", context, f)
		}
		if r := path["D"].ReplyTo; r != path["A"].ID {
			t.Errorf("%s: D reply_to = %q, want A's stored id %q", context, r, path["A"].ID)
		}
	}
	assertTree(t, s, "live")

	// ── Persist, then reload through a FRESH store instance ───────────
	if err := st.Persist(s); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	st2 := NewChatSessionStore(dir, backend)
	s2 := st2.GetOrCreate("sess-1")
	if len(s2.SnapshotMessages()) == 0 {
		t.Fatal("reload: session empty — load failed")
	}
	assertTree(t, s2, "reloaded")

	// ── The client-id map must survive the reload ─────────────────────
	// A further turn chaining onto optD (a client id from before the
	// reload) must still resolve to D's stored id.
	storedD := s2.ActiveLeafID
	if !s2.AppendUserMessage("optE", "E", "optD", "") {
		t.Fatal("append E rejected")
	}
	msgs := s2.SnapshotMessages()
	last := msgs[len(msgs)-1]
	if last.Content != "E" {
		t.Fatalf("last message = %q, want E", last.Content)
	}
	if last.ParentID != storedD {
		t.Errorf("E parent = %q, want D's stored id %q (client-id map lost on reload)", last.ParentID, storedD)
	}
	if last.ForkedFrom != "" {
		t.Errorf("E should not be a fork, got forked_from=%q", last.ForkedFrom)
	}
}

// Unknown client ids must pass through unchanged (cross-branch quotes and
// stale ids degrade gracefully instead of being dropped).
func TestResolveClientID_UnknownPassesThrough(t *testing.T) {
	s := &ChatSession{Messages: map[string]*ChatMessage{"stored1": {ID: "stored1"}}}
	if got := s.resolveClientIDLocked("stored1"); got != "stored1" {
		t.Errorf("stored id should pass through, got %q", got)
	}
	if got := s.resolveClientIDLocked("never-seen"); got != "never-seen" {
		t.Errorf("unknown id should pass through, got %q", got)
	}
	if got := s.resolveClientIDLocked(""); got != "" {
		t.Errorf("empty should stay empty, got %q", got)
	}
}
