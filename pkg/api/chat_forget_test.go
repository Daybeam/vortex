package api

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/store"

	_ "modernc.org/sqlite"
)

// ─── DeleteTaskArtifacts Tests ─────────────────────────────────────────────

// TestDeleteTaskArtifacts_ValidTaskID removes the directory and returns nil.
func TestDeleteTaskArtifacts_ValidTaskID(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "task123")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	// Create a file inside to verify removal.
	if err := os.WriteFile(filepath.Join(taskDir, "artifact.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	if err := DeleteTaskArtifacts(root, "task123"); err != nil {
		t.Fatalf("DeleteTaskArtifacts failed: %v", err)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Error("task directory should be removed")
	}
}

// TestDeleteTaskArtifacts_NonExistent returns nil (os.RemoveAll is idempotent).
func TestDeleteTaskArtifacts_NonExistent(t *testing.T) {
	root := t.TempDir()
	if err := DeleteTaskArtifacts(root, "nonexistent"); err != nil {
		t.Errorf("expected nil for non-existent task, got %v", err)
	}
}

// TestDeleteTaskArtifacts_PathSeparator rejected to prevent traversal.
func TestDeleteTaskArtifacts_PathSeparator(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"a/b", "a\\b", "..", "a/../b", ""} {
		if err := DeleteTaskArtifacts(root, bad); err == nil {
			t.Errorf("expected error for taskID %q, got nil", bad)
		}
	}
}

// ─── handleForget: task scope Tests ────────────────────────────────────────

// newForgetTestHandler creates a ChatHandler with Archive + OutputBase for
// task-scope /forget tests.
func newForgetTestHandler(t *testing.T) *ChatHandler {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.jsonl")
	archive := core.NewContextArchive(archivePath)
	// Append some items (with Summary so SearchRecentStepSummaries can find them).
	_ = archive.Append(core.MemoryItem{TaskID: "taskA", NodeID: "n1", Intent: "alpha", Summary: "did alpha", Timestamp: time.Now()})
	_ = archive.Append(core.MemoryItem{TaskID: "taskB", NodeID: "n2", Intent: "beta", Summary: "did beta", Timestamp: time.Now()})

	outputBase := filepath.Join(dir, "outputs")
	_ = os.MkdirAll(filepath.Join(outputBase, "taskA"), 0755)

	return &ChatHandler{
		Harness: &chat.ChatHarness{
			Archive:    archive,
			OutputBase: outputBase,
		},
	}
}

// TestHandleForget_NoArgs shows usage text.
func TestHandleForget_NoArgs(t *testing.T) {
	h := newForgetTestHandler(t)
	handled, resp := h.handleSlashCommand("/forget", &chat.ChatSession{}, "sess1", "admin")
	if !handled {
		t.Error("should be handled")
	}
	if resp == "" || !containsStr(resp, "Usage:") {
		t.Errorf("expected usage text, got: %s", resp)
	}
}

// TestHandleForget_InvalidScope shows usage text.
func TestHandleForget_InvalidScope(t *testing.T) {
	h := newForgetTestHandler(t)
	handled, resp := h.handleSlashCommand("/forget bogus", &chat.ChatSession{}, "sess1", "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "Usage:") {
		t.Errorf("expected usage text for invalid scope, got: %s", resp)
	}
}

// TestHandleForget_Task_Step1_Challenge issues a confirmation challenge.
func TestHandleForget_Task_Step1_Challenge(t *testing.T) {
	h := newForgetTestHandler(t)
	handled, resp := h.handleSlashCommand("/forget task taskA", &chat.ChatSession{}, "sess1", "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "⚠️") {
		t.Errorf("expected challenge warning, got: %s", resp)
	}
	if !containsStr(resp, "confirm") {
		t.Errorf("expected 'confirm' instruction, got: %s", resp)
	}
	if !containsStr(resp, "60") {
		t.Errorf("expected 60s window, got: %s", resp)
	}
}

// TestHandleForget_Task_Step2_Confirm executes deletion.
func TestHandleForget_Task_Step2_Confirm(t *testing.T) {
	h := newForgetTestHandler(t)
	session := &chat.ChatSession{}
	sessionID := "sess1"

	// Step 1: issue challenge.
	h.handleSlashCommand("/forget task taskA", session, sessionID, "admin")

	// Step 2: confirm.
	handled, resp := h.handleSlashCommand("/forget task taskA confirm", session, sessionID, "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "✅") {
		t.Errorf("expected success, got: %s", resp)
	}

	// Verify archive entries for taskA are gone.
	items := h.Harness.Archive.SearchRecentStepSummaries("taskA", "default", 100)
	if len(items) != 0 {
		t.Errorf("expected 0 taskA items after deletion, got %d", len(items))
	}
	// Verify taskB items survive.
	items = h.Harness.Archive.SearchRecentStepSummaries("taskB", "default", 100)
	if len(items) != 1 {
		t.Errorf("expected 1 taskB item to survive, got %d", len(items))
	}
}

// TestHandleForget_Task_NoPendingConfirmation returns error.
func TestHandleForget_Task_NoPendingConfirmation(t *testing.T) {
	h := newForgetTestHandler(t)
	handled, resp := h.handleSlashCommand("/forget task taskA confirm", &chat.ChatSession{}, "sess1", "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "❌") {
		t.Errorf("expected error for no pending confirmation, got: %s", resp)
	}
}

// TestHandleForget_Task_ExpiredConfirmation returns error.
func TestHandleForget_Task_ExpiredConfirmation(t *testing.T) {
	h := newForgetTestHandler(t)
	sessionID := "sess1"

	// Manually store an expired challenge.
	key := forgetChallengeKey(sessionID, "task", "taskA")
	h.forgetConfirmations.Store(key, time.Now().Unix()-1) // already expired

	handled, resp := h.handleSlashCommand("/forget task taskA confirm", &chat.ChatSession{}, sessionID, "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "expired") {
		t.Errorf("expected expired error, got: %s", resp)
	}
}

// ─── handleForget: user + memory scope Tests ───────────────────────────────

// newForgetTestHandlerWithDB creates a ChatHandler with MemoryBank backed by
// an in-memory SQLite DB for user/memory scope tests.
func newForgetTestHandlerWithDB(t *testing.T) *ChatHandler {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	t.Cleanup(func() { db.Close() }) // audit CRITICAL-002: prevent DB connection leak

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS memory_bank (
		category TEXT,
		key TEXT,
		content TEXT,
		metadata_json TEXT,
		embedding BLOB,
		embedding_model TEXT,
		last_updated DATETIME,
		PRIMARY KEY (category, key)
	)`)
	if err != nil {
		t.Fatalf("CREATE TABLE failed: %v", err)
	}

	// Insert test data.
	_, err = db.Exec(`INSERT INTO memory_bank (category, key, content, last_updated) VALUES (?, ?, ?, ?)`,
		"user_profile", "raw", "## Preferences\n- prefers Go\n- uses dark mode", time.Now())
	if err != nil {
		t.Fatalf("INSERT failed: %v", err)
	}
	_, err = db.Exec(`INSERT INTO memory_bank (category, key, content, last_updated) VALUES (?, ?, ?, ?)`,
		"decisions", "raw", "## Decisions\n- use SQLite for storage", time.Now())
	if err != nil {
		t.Fatalf("INSERT failed: %v", err)
	}

	backend := store.NewSQLiteMemoryBankBackend(db)
	dir := t.TempDir()
	mbStore := store.NewMemoryBankStore(dir, backend)

	return &ChatHandler{
		Harness: &chat.ChatHarness{
			MemoryBank: mbStore,
		},
	}
}

// TestHandleForget_User_Confirm deletes user-scoped MemoryBank entries.
func TestHandleForget_User_Confirm(t *testing.T) {
	h := newForgetTestHandlerWithDB(t)
	sessionID := "sess1"

	// Step 1: challenge.
	h.handleSlashCommand("/forget user", &chat.ChatSession{}, sessionID, "admin")

	// Step 2: confirm.
	handled, resp := h.handleSlashCommand("/forget user confirm", &chat.ChatSession{}, sessionID, "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "✅") {
		t.Errorf("expected success, got: %s", resp)
	}
	// We inserted 1 user_profile entry → response should say "1".
	if !containsStr(resp, "1") {
		t.Errorf("expected 1 deleted, got: %s", resp)
	}
}

// TestHandleForget_Memory_Confirm search-and-deletes matching entries.
func TestHandleForget_Memory_Confirm(t *testing.T) {
	h := newForgetTestHandlerWithDB(t)
	sessionID := "sess1"

	// Step 1: challenge.
	h.handleSlashCommand("/forget memory SQLite", &chat.ChatSession{}, sessionID, "admin")

	// Step 2: confirm.
	handled, resp := h.handleSlashCommand("/forget memory SQLite confirm", &chat.ChatSession{}, sessionID, "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "✅") {
		t.Errorf("expected success, got: %s", resp)
	}
	// The "decisions" entry contains "SQLite" → should be deleted (1 entry).
	if !containsStr(resp, "1") {
		t.Errorf("expected 1 deleted, got: %s", resp)
	}
}

// TestHandleForget_Memory_NoMatch deletes nothing.
func TestHandleForget_Memory_NoMatch(t *testing.T) {
	h := newForgetTestHandlerWithDB(t)
	sessionID := "sess1"

	h.handleSlashCommand("/forget memory nonexistent", &chat.ChatSession{}, sessionID, "admin")
	handled, resp := h.handleSlashCommand("/forget memory nonexistent confirm", &chat.ChatSession{}, sessionID, "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "0") {
		t.Errorf("expected 0 deleted for no match, got: %s", resp)
	}
}

// TestHandleForget_Memory_SecondDelete verifies the deletion is persistent:
// after deleting "SQLite", a second /forget memory SQLite finds 0 entries.
func TestHandleForget_Memory_SecondDeleteFindsZero(t *testing.T) {
	h := newForgetTestHandlerWithDB(t)
	sessionID := "sess1"

	// First deletion.
	h.handleSlashCommand("/forget memory SQLite", &chat.ChatSession{}, sessionID, "admin")
	h.handleSlashCommand("/forget memory SQLite confirm", &chat.ChatSession{}, sessionID, "admin")

	// Second deletion should find 0.
	h.handleSlashCommand("/forget memory SQLite", &chat.ChatSession{}, sessionID, "admin")
	handled, resp := h.handleSlashCommand("/forget memory SQLite confirm", &chat.ChatSession{}, sessionID, "admin")
	if !handled {
		t.Error("should be handled")
	}
	if !containsStr(resp, "0") {
		t.Errorf("expected 0 on second deletion, got: %s", resp)
	}
}

// containsStr is a helper to avoid collision with core.contains.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
