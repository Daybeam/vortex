package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — inbox owner
// filtering happened after SQL LIMIT, so foreign rows could crowd out owned ones.
func TestChatOutbox_OwnerScopePrecedesLimit(t *testing.T) {
	o := NewSQLiteChatOutbox(newOutboxTestDB(t))
	ctx := context.Background()
	for _, item := range []OutboxItem{
		{OwnerID: "bob", Kind: "schedule_inbox", DedupeKey: "bob-1", Text: "bob"},
		{OwnerID: "bob", Kind: "schedule_inbox", DedupeKey: "bob-2", Text: "bob"},
		{OwnerID: "alice", Kind: "schedule_inbox", DedupeKey: "alice-1", Text: "alice"},
		{Kind: "schedule_inbox", DedupeKey: "legacy", Text: "legacy"},
	} {
		if _, err := o.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	items, err := o.ListPendingKindsScoped(ctx, []string{"schedule_inbox"}, "alice", false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].OwnerID != "alice" || items[0].DedupeKey != "alice-1" {
		t.Fatalf("alice page = %+v, want alice-1", items)
	}
	items, err = o.ListPendingKindsScoped(ctx, []string{"schedule_inbox"}, "", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].DedupeKey != "legacy" {
		t.Fatalf("anonymous page = %+v, want only legacy", items)
	}
	items, err = o.ListPendingKindsScoped(ctx, []string{"schedule_inbox"}, "alice", true, 10)
	if err != nil || len(items) != 4 {
		t.Fatalf("admin/local positive control: items=%+v err=%v", items, err)
	}
}

func TestChatOutbox_LegacyInboxOwnerBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chat_outbox (kind, dedupe_key, text) VALUES (?, ?, ?)`,
		"schedule_inbox", "legacy-json", `{"delivery":{"owner_id":"alice"},"task_id":"task"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chat_outbox (kind, dedupe_key, text) VALUES (?, ?, ?)`,
		"schedule_inbox", "legacy-empty", `{"delivery":{},"task_id":"task"}`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for key, want := range map[string]string{"legacy-json": "alice", "legacy-empty": ""} {
		var owner string
		if err := db.QueryRow(`SELECT owner_id FROM chat_outbox WHERE dedupe_key = ?`, key).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != want {
			t.Errorf("%s owner=%q, want %q", key, owner, want)
		}
	}
}
