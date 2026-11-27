package chat

import (
	"context"
	"database/sql"
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
	if !s1.AppendUserMessage("msg1", "hello", "", "") {
		t.Fatal("failed to append user message")
	}
	if err := st.Persist(s1); err != nil {
		t.Fatalf("persist s1: %v", err)
	}

	// Small delay so mod times differ
	time.Sleep(20 * time.Millisecond)

	s2 := st.GetOrCreate("session-bbb")
	if !s2.AppendUserMessage("msg2", "world", "", "") {
		t.Fatal("failed to append user message")
	}
	if err := st.Persist(s2); err != nil {
		t.Fatalf("persist s2: %v", err)
	}

	// ListSessions should return both sessions from files, newest first
	rows, err := st.ListSessions(context.Background(), store.SessionScope{All: true}, 50, 0)
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
		s.AppendUserMessage("msg", "content", "", "")
		st.Persist(s)
		time.Sleep(10 * time.Millisecond)
	}

	rows, err := st.ListSessions(context.Background(), store.SessionScope{All: true}, 2, 0)
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

	rows, err := st.ListSessions(context.Background(), store.SessionScope{All: true}, 50, 0)
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

	rows, err := st.ListSessions(context.Background(), store.SessionScope{All: true}, 50, 0)
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

	rows, err := st.ListSessions(context.Background(), store.SessionScope{All: true}, 50, 0)
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 sessions from file fallback, got %d", len(rows))
	}
}

// TestChatSessionOwnership_JSONRoundTripAndScopedFallback is a regression test
// for docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md: owner/shared metadata
// must survive JSON persistence and foreign files must be filtered before limit.
func TestChatSessionOwnership_JSONRoundTripAndScopedFallback(t *testing.T) {
	dir := t.TempDir()
	st := NewChatSessionStore(dir, nil)
	allow := func(*ChatSession) error { return nil }
	for _, item := range []struct {
		id, owner string
		shared    bool
	}{{"foreign-1", "bob", false}, {"foreign-2", "bob", true}, {"alice-1", "alice", true}} {
		s, err := st.GetOrCreateOwned(item.id, item.owner, true, allow)
		if err != nil {
			t.Fatal(err)
		}
		s.SetShared(item.shared)
		s.AppendUserMessage("m-"+item.id, item.id, "", "")
		if err := st.Persist(s); err != nil {
			t.Fatal(err)
		}
	}
	reloaded := NewChatSessionStore(dir, nil)
	s := reloaded.Load("alice-1")
	if s == nil || s.GetOwnerID() != "alice" || !s.IsShared() {
		t.Fatalf("JSON metadata round trip failed: %+v", s)
	}
	rows, err := reloaded.ListSessions(context.Background(), store.SessionScope{Owner: "alice"}, 1, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != "alice-1" || rows[0].OwnerID != "alice" || !rows[0].Shared {
		t.Fatalf("scoped file fallback = %+v, %v", rows, err)
	}
	rows, err = reloaded.ListSessions(context.Background(), store.SessionScope{Owner: "nobody"}, 1, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("exact scope leaked files: %+v, %v", rows, err)
	}
}

// TestChatSessionOwnership_AtomicCreateAndStableOwner is a regression test for
// docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md: concurrent get/create must
// authorize one immutable owner, and a required blank owner must not create data.
func TestChatSessionOwnership_AtomicCreateAndStableOwner(t *testing.T) {
	st := NewChatSessionStore(t.TempDir(), nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, owner := range []string{"alice", "bob"} {
		owner := owner
		go func() {
			<-start
			_, err := st.GetOrCreateOwned("contended", owner, true, func(s *ChatSession) error {
				if s.GetOwnerID() != owner {
					return context.Canceled // deterministic denial sentinel
				}
				return nil
			})
			results <- err
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("atomic owner create successes = %d, want 1", successes)
	}
	if s := st.Load("contended"); s == nil || (s.GetOwnerID() != "alice" && s.GetOwnerID() != "bob") {
		t.Fatalf("unexpected contended owner: %+v", s)
	}
	if s, err := st.GetOrCreateOwned("blank", "", true, nil); err == nil || s != nil || st.Load("blank") != nil {
		t.Fatalf("blank required owner created a session: session=%+v err=%v", s, err)
	}
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — a successful
// SQLite metadata row with zero messages must not suppress a valid JSON
// transcript. SaveMessage can fail after SaveSession succeeds, leaving exactly
// this partial-write state; reload must recover the JSON messages.
func TestLoadSession_MetadataOnlyDBFallsBackToJSON(t *testing.T) {
	dir := t.TempDir()
	id := "partial-db-session"
	raw := `{"id":"partial-db-session","root_id":"m1","active_leaf_id":"m1","owner_id":"alice","messages":{"m1":{"id":"m1","role":"user","content":"recover me"}}}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	backend := &fakeListBackend{sessions: []store.ChatSessionRow{{ID: id, RootID: "m1", ActiveLeafID: "m1", OwnerID: "alice"}}}
	st := NewChatSessionStore(dir, backend)
	s := st.Load(id)
	if s == nil {
		t.Fatal("metadata-only DB row suppressed JSON fallback")
	}
	msgs := s.SnapshotMessages()
	if len(msgs) != 1 || msgs[0].Content != "recover me" || s.GetOwnerID() != "alice" {
		t.Fatalf("JSON fallback did not restore transcript/metadata: messages=%+v owner=%q", msgs, s.GetOwnerID())
	}
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — JSON is a
// transcript fallback, not a prerequisite for a real empty DB session.
func TestLoadSession_EmptyDBSessionWithoutJSONRemainsLoadable(t *testing.T) {
	id := "empty-db-session"
	backend := &fakeListBackend{sessions: []store.ChatSessionRow{{ID: id, OwnerID: "alice"}}}
	st := NewChatSessionStore(t.TempDir(), backend)
	s := st.Load(id)
	if s == nil || s.GetOwnerID() != "alice" || len(s.SnapshotMessages()) != 0 {
		t.Fatalf("empty DB session was lost during JSON fallback: %+v", s)
	}
}

// fakeListBackend implements store.IChatBackend for ListSessions testing.
type fakeListBackend struct {
	sessions []store.ChatSessionRow
}

func (f *fakeListBackend) ListSessions(ctx context.Context, scope store.SessionScope, limit, offset int) ([]store.ChatSessionRow, error) {
	var sessions []store.ChatSessionRow
	for _, row := range f.sessions {
		if scope.All || row.OwnerID == scope.Owner {
			sessions = append(sessions, row)
		}
	}
	if offset > 0 && offset < len(sessions) {
		s := sessions[offset:]
		if limit > 0 && len(s) > limit {
			return s[:limit], nil
		}
		return s, nil
	} else if offset > 0 {
		return []store.ChatSessionRow{}, nil
	}
	if limit > 0 && len(sessions) > limit {
		return sessions[:limit], nil
	}
	return sessions, nil
}
func (f *fakeListBackend) SaveSession(ctx context.Context, id, rootID, leafID, ownerID string, shared bool, createdAt time.Time) error {
	return nil
}
func (f *fakeListBackend) SaveMessage(ctx context.Context, msg *store.ChatMessageRow) error {
	return nil
}
func (f *fakeListBackend) LoadSession(ctx context.Context, sessionID string) (rootID, leafID, ownerID string, shared bool, msgs map[string]*store.ChatMessageRow, err error) {
	for _, row := range f.sessions {
		if row.ID == sessionID {
			return row.RootID, row.ActiveLeafID, row.OwnerID, row.Shared, map[string]*store.ChatMessageRow{}, nil
		}
	}
	return "", "", "", false, nil, sql.ErrNoRows
}
func (f *fakeListBackend) SavePendingReply(ctx context.Context, row store.PendingReplyRow) error {
	return nil
}
func (f *fakeListBackend) ClearPendingReply(ctx context.Context, sessionID string) error {
	return nil
}
func (f *fakeListBackend) ListPendingReplies(ctx context.Context) ([]store.PendingReplyRow, error) {
	return nil, nil
}
