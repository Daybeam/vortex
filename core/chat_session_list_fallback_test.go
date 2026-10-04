package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/store"
)

// TestListSessions_FileFallback is a regression test for the bug where
// ListSessions returned nil when no DB backend was configured, even though
// session JSON files existed on disk. This made the chat History button
// always show "No past sessions" in standalone mode (no SQLite).
//
// Bug: ListSessions only queried the DB backend. With no DB, sessions were
// persisted as JSON files but never listed.
// Fix: Fall back to scanning the session directory for *.json files.
func TestListSessions_FileFallback(t *testing.T) {
	// Create a temp dir for session files
	dir := t.TempDir()
	st := NewChatSessionStore(dir, nil) // nil backend = file-based mode

	// Create two sessions with messages
	s1 := st.GetOrCreate("session-aaa")
	if !s1.AppendUserMessage("msg1", "hello", "") {
		t.Fatal("failed to append user message")
	}
	if err := st.Persist(s1); err != nil {
		t.Fatalf("persist s1: %v", err)
	}

	// Small delay so mod times differ
	time.Sleep(20 * time.Millisecond)

	s2 := st.GetOrCreate("session-bbb")
	if !s2.AppendUserMessage("msg2", "world", "") {
		t.Fatal("failed to append user message")
	}
	if err := st.Persist(s2); err != nil {
		t.Fatalf("persist s2: %v", err)
	}

	// ListSessions should return both sessions from files, newest first
	rows, err := st.ListSessions(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(rows))
	}

	// Should be sorted by mod time descending (newest first)
	if rows[0].ID != "session-bbb" {
		t.Errorf("expected newest session 'session-bbb' first, got %q", rows[0].ID)
	}
	if rows[1].ID != "session-aaa" {
		t.Errorf("expected older session 'session-aaa' second, got %q", rows[1].ID)
	}

	// Verify CreatedAt is populated from file mod time
	for _, row := range rows {
		if row.CreatedAt.IsZero() {
			t.Errorf("session %q has zero CreatedAt", row.ID)
		}
	}
}

// TestListSessions_FileFallback_Limit verifies the limit parameter is
// respected when using the file-based fallback.
func TestListSessions_FileFallback_Limit(t *testing.T) {
	dir := t.TempDir()
	st := NewChatSessionStore(dir, nil)

	// Create 3 sessions
	for i := 0; i < 3; i++ {
		s := st.GetOrCreate("session-" + string(rune('a'+i)))
		s.AppendUserMessage("msg", "content", "")
		st.Persist(s)
		time.Sleep(10 * time.Millisecond)
	}

	rows, err := st.ListSessions(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 sessions with limit=2, got %d", len(rows))
	}
}

// TestListSessions_FileFallback_EmptyDir verifies no error when dir is empty.
func TestListSessions_FileFallback_EmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	st := NewChatSessionStore(dir, nil)

	rows, err := st.ListSessions(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("ListSessions error on empty dir: %v", err)
	}
	if rows != nil && len(rows) != 0 {
		t.Fatalf("expected nil/empty rows, got %d", len(rows))
	}
}

// TestListSessions_DBBackendPreferred verifies that when a DB backend is
// configured and returns data, the file-based fallback is NOT used.
func TestListSessions_DBBackendPreferred(t *testing.T) {
	dir := t.TempDir()

	// Create a fake backend that returns a known session
	fake := &fakeListBackend{
		sessions: []store.ChatSessionRow{
			{ID: "db-session", CreatedAt: time.Now()},
		},
	}

	st := NewChatSessionStore(dir, fake)

	// Also create a file-based session that should NOT appear
	os.WriteFile(filepath.Join(dir, "file-session.json"), []byte("{}"), 0644)

	rows, err := st.ListSessions(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session from DB, got %d", len(rows))
	}
	if rows[0].ID != "db-session" {
		t.Errorf("expected 'db-session', got %q", rows[0].ID)
	}
}

// TestListSessions_DBEmptyFallsBackToFiles verifies that when a DB backend
// is configured but returns 0 sessions, the file-based fallback kicks in.
// This handles the case where SaveSession silently fails (e.g. lifecycle
// context cancelled) but JSON files were still written to disk.
func TestListSessions_DBEmptyFallsBackToFiles(t *testing.T) {
	dir := t.TempDir()

	// DB backend returns empty (simulating SaveSession failures)
	fake := &fakeListBackend{
		sessions: []store.ChatSessionRow{}, // empty
	}

	st := NewChatSessionStore(dir, fake)

	// Create file-based sessions that SHOULD appear via fallback
	os.WriteFile(filepath.Join(dir, "file-session-1.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "file-session-2.json"), []byte("{}"), 0644)

	rows, err := st.ListSessions(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 sessions from file fallback, got %d", len(rows))
	}
}

// fakeListBackend implements store.IChatBackend for ListSessions testing.
type fakeListBackend struct {
	sessions []store.ChatSessionRow
}

func (f *fakeListBackend) ListSessions(ctx context.Context, limit, offset int) ([]store.ChatSessionRow, error) {
	if offset > 0 && offset < len(f.sessions) {
		s := f.sessions[offset:]
		if limit > 0 && len(s) > limit {
			return s[:limit], nil
		}
		return s, nil
	} else if offset > 0 {
		return []store.ChatSessionRow{}, nil
	}
	if limit > 0 && len(f.sessions) > limit {
		return f.sessions[:limit], nil
	}
	return f.sessions, nil
}
func (f *fakeListBackend) SaveSession(ctx context.Context, id, rootID, leafID string, createdAt time.Time) error {
	return nil
}
func (f *fakeListBackend) SaveMessage(ctx context.Context, msgID, sessionID, parentID, role, content string, createdAt time.Time) error {
	return nil
}
func (f *fakeListBackend) LoadSession(ctx context.Context, sessionID string) (rootID, leafID string, msgs map[string]*store.ChatMessageRow, err error) {
	return "", "", nil, nil
}
