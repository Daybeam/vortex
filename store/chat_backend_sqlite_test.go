package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestSQLiteChatBackend_ReplyAndForkRoundTrip verifies the Mode 1 (fork) and
// Mode 2 (quote) markers survive a save/load cycle through SQLite. These
// columns back the Telegram-style quote overlay and the fork connector line
// in the chat UI.
func TestSQLiteChatBackend_ReplyAndForkRoundTrip(t *testing.T) {
	db, err := InitDB(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteChatBackend(db)
	ctx := context.Background()
	now := time.Now().Round(time.Second)

	if err := backend.SaveSession(ctx, "s1", "m1", "m3", now); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}
	// m1: root, no markers
	if err := backend.SaveMessage(ctx, &ChatMessageRow{ID: "m1", SessionID: "s1", Role: "user", Content: "hello", CreatedAt: now}); err != nil {
		t.Fatalf("SaveMessage m1 failed: %v", err)
	}
	// m2: quotes m1 (Mode 2)
	if err := backend.SaveMessage(ctx, &ChatMessageRow{ID: "m2", SessionID: "s1", ParentID: "m1", ClientID: "c2", ReplyTo: "m1", Role: "user", Content: "reply to hello", CreatedAt: now}); err != nil {
		t.Fatalf("SaveMessage m2 failed: %v", err)
	}
	// m3: forked from m1 (Mode 1)
	if err := backend.SaveMessage(ctx, &ChatMessageRow{ID: "m3", SessionID: "s1", ParentID: "m1", ClientID: "c3", ForkedFrom: "m1", Role: "user", Content: "branch from hello", CreatedAt: now}); err != nil {
		t.Fatalf("SaveMessage m3 failed: %v", err)
	}

	root, leaf, msgs, err := backend.LoadSession(ctx, "s1")
	if err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}
	if root != "m1" || leaf != "m3" {
		t.Fatalf("expected root=m1 leaf=m3, got root=%q leaf=%q", root, leaf)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs["m1"].ReplyTo != "" || msgs["m1"].ForkedFrom != "" {
		t.Errorf("m1 should have no markers, got reply_to=%q forked_from=%q", msgs["m1"].ReplyTo, msgs["m1"].ForkedFrom)
	}
	if msgs["m2"].ReplyTo != "m1" {
		t.Errorf("m2 reply_to: expected m1, got %q", msgs["m2"].ReplyTo)
	}
	if msgs["m3"].ForkedFrom != "m1" {
		t.Errorf("m3 forked_from: expected m1, got %q", msgs["m3"].ForkedFrom)
	}
	// Client ids round-trip so later turns referencing them can resolve.
	if msgs["m2"].ClientID != "c2" || msgs["m3"].ClientID != "c3" {
		t.Errorf("client ids lost: m2=%q m3=%q", msgs["m2"].ClientID, msgs["m3"].ClientID)
	}
}

// TestSQLiteChatBackend_LegacySchemaMigration covers databases created before
// the reply_to/forked_from columns existed: InitDB's best-effort ALTERs must
// backfill them so SaveMessage/LoadSession keep working on old data.
func TestSQLiteChatBackend_LegacySchemaMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open legacy db: %v", err)
	}
	legacyStmts := `
	CREATE TABLE chat_sessions (id TEXT PRIMARY KEY, root_id TEXT, active_leaf_id TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE chat_messages (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, parent_id TEXT, role TEXT NOT NULL, content TEXT NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
	`
	if _, err := legacy.Exec(legacyStmts); err != nil {
		t.Fatalf("failed to create legacy schema: %v", err)
	}
	legacy.Close()

	// Reopen through InitDB: CREATE TABLE IF NOT EXISTS is a no-op, and the
	// idempotent ALTERs add the new columns to the existing table.
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB on legacy db failed: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteChatBackend(db)
	ctx := context.Background()
	now := time.Now().Round(time.Second)
	if err := backend.SaveSession(ctx, "s1", "m1", "m2", now); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}
	if err := backend.SaveMessage(ctx, &ChatMessageRow{ID: "m1", SessionID: "s1", ClientID: "c1", Role: "user", Content: "hello", CreatedAt: now}); err != nil {
		t.Fatalf("SaveMessage m1 failed: %v", err)
	}
	if err := backend.SaveMessage(ctx, &ChatMessageRow{ID: "m2", SessionID: "s1", ParentID: "m1", ClientID: "c2", ReplyTo: "m1", ForkedFrom: "m1", Role: "user", Content: "quote+fork", CreatedAt: now}); err != nil {
		t.Fatalf("SaveMessage m2 failed: %v", err)
	}
	_, _, msgs, err := backend.LoadSession(ctx, "s1")
	if err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}
	if msgs["m2"].ReplyTo != "m1" || msgs["m2"].ForkedFrom != "m1" {
		t.Errorf("markers lost after migration: reply_to=%q forked_from=%q", msgs["m2"].ReplyTo, msgs["m2"].ForkedFrom)
	}
	if msgs["m2"].ClientID != "c2" {
		t.Errorf("client_id lost after migration: %q", msgs["m2"].ClientID)
	}
}
