package api

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/store"
)

// TestScanPendingReplies_EnqueuesInterrupt verifies that the startup scan
// enqueues a chat_interrupted notification for a pending reply that has no
// corresponding chat_reply in the outbox (i.e., the turn was interrupted
// before the reply was produced).
func TestScanPendingReplies_EnqueuesInterrupt(t *testing.T) {
	hb := newHub(t, t.TempDir()+"/test.db")
	ctx := context.Background()

	// Simulate a crash: write a pending reply directly to the DB,
	// aged past the 2-minute threshold.
	sessionID := "telegram_123"
	msgID := "msg_001"
	hb.db.ExecContext(ctx, `
		INSERT INTO chat_pending_replies (session_id, message_id, content, started_at)
		VALUES (?, ?, ?, ?)
	`, sessionID, msgID, "hello", time.Now().Add(-5*time.Minute))

	if err := hb.h.ScanPendingReplies(ctx); err != nil {
		t.Fatalf("ScanPendingReplies: %v", err)
	}

	// The chat_interrupted notification should be in the outbox.
	exists, err := hb.outbox.RowExists(ctx, "chat_interrupted:"+sessionID+":"+msgID)
	if err != nil {
		t.Fatalf("RowExists: %v", err)
	}
	if !exists {
		t.Error("expected chat_interrupted row in outbox, not found")
	}

	// The pending reply should be cleared.
	pending, err := hb.h.Store.ListPendingReplies(ctx)
	if err != nil {
		t.Fatalf("ListPendingReplies: %v", err)
	}
	for _, p := range pending {
		if p.SessionID == sessionID {
			t.Error("expected pending reply to be cleared after scan")
		}
	}
}

// TestScanPendingReplies_ClearsWhenReplyExists verifies that the startup scan
// clears a pending reply without enqueuing chat_interrupted when the
// corresponding chat_reply already exists in the outbox (i.e., the turn
// completed and the reply was enqueued before the crash).
func TestScanPendingReplies_ClearsWhenReplyExists(t *testing.T) {
	hb := newHub(t, t.TempDir()+"/test.db")
	ctx := context.Background()

	sessionID := "telegram_456"
	msgID := "msg_002"

	// Simulate: turn completed, chat_reply was enqueued, but the process
	// died before clearing the pending mark.
	hb.outbox.Enqueue(ctx, store.OutboxItem{
		SessionID: sessionID,
		Kind:      kindChatReply,
		DedupeKey: "chat_reply:" + sessionID + ":" + msgID,
		Text:      "the reply",
	})
	hb.db.ExecContext(ctx, `
		INSERT INTO chat_pending_replies (session_id, message_id, content, started_at)
		VALUES (?, ?, ?, ?)
	`, sessionID, msgID, "hello", time.Now().Add(-5*time.Minute))

	if err := hb.h.ScanPendingReplies(ctx); err != nil {
		t.Fatalf("ScanPendingReplies: %v", err)
	}

	// No chat_interrupted should be enqueued.
	exists, err := hb.outbox.RowExists(ctx, "chat_interrupted:"+sessionID+":"+msgID)
	if err != nil {
		t.Fatalf("RowExists: %v", err)
	}
	if exists {
		t.Error("did not expect chat_interrupted row when chat_reply exists")
	}

	// The pending reply should be cleared.
	pending, err := hb.h.Store.ListPendingReplies(ctx)
	if err != nil {
		t.Fatalf("ListPendingReplies: %v", err)
	}
	for _, p := range pending {
		if p.SessionID == sessionID {
			t.Error("expected pending reply to be cleared after scan")
		}
	}
}

// TestScanPendingReplies_SkipsRecentPending verifies that the startup scan
// skips pending replies that are younger than the age threshold, as these
// may belong to turns that were in progress during a graceful restart.
func TestScanPendingReplies_SkipsRecentPending(t *testing.T) {
	hb := newHub(t, t.TempDir()+"/test.db")
	ctx := context.Background()

	sessionID := "telegram_789"
	msgID := "msg_003"

	// Write a pending reply that is only 30 seconds old (under threshold).
	hb.db.ExecContext(ctx, `
		INSERT INTO chat_pending_replies (session_id, message_id, content, started_at)
		VALUES (?, ?, ?, ?)
	`, sessionID, msgID, "hello", time.Now().Add(-30*time.Second))

	if err := hb.h.ScanPendingReplies(ctx); err != nil {
		t.Fatalf("ScanPendingReplies: %v", err)
	}

	// No chat_interrupted should be enqueued (skipped due to age).
	exists, err := hb.outbox.RowExists(ctx, "chat_interrupted:"+sessionID+":"+msgID)
	if err != nil {
		t.Fatalf("RowExists: %v", err)
	}
	if exists {
		t.Error("did not expect chat_interrupted for recent pending reply")
	}

	// The pending reply should still be there (not cleared).
	pending, err := hb.h.Store.ListPendingReplies(ctx)
	if err != nil {
		t.Fatalf("ListPendingReplies: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.SessionID == sessionID {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected pending reply to remain (skipped due to age threshold)")
	}
}

// TestChatReplyDedupe verifies that enqueuing a chat_reply with the same
// dedupe_key twice returns false the second time (no duplicate delivery).
func TestChatReplyDedupe(t *testing.T) {
	hb := newHub(t, t.TempDir()+"/test.db")
	ctx := context.Background()

	sessionID := "telegram_dedupe"
	msgID := "msg_004"
	dedupeKey := "chat_reply:" + sessionID + ":" + msgID

	added1, err := hb.outbox.Enqueue(ctx, store.OutboxItem{
		SessionID: sessionID,
		Kind:      kindChatReply,
		DedupeKey: dedupeKey,
		Text:      "first reply",
	})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if !added1 {
		t.Error("expected first Enqueue to return true")
	}

	added2, err := hb.outbox.Enqueue(ctx, store.OutboxItem{
		SessionID: sessionID,
		Kind:      kindChatReply,
		DedupeKey: dedupeKey,
		Text:      "second reply",
	})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if added2 {
		t.Error("expected second Enqueue to return false (dedupe)")
	}
}

// TestRowExists verifies the RowExists method on the outbox.
func TestRowExists(t *testing.T) {
	hb := newHub(t, t.TempDir()+"/test.db")
	ctx := context.Background()

	// No row exists initially.
	exists, err := hb.outbox.RowExists(ctx, "nonexistent_key")
	if err != nil {
		t.Fatalf("RowExists: %v", err)
	}
	if exists {
		t.Error("expected RowExists to return false for nonexistent key")
	}

	// Enqueue a row.
	hb.outbox.Enqueue(ctx, store.OutboxItem{
		SessionID: "telegram_row",
		Kind:      kindChatReply,
		DedupeKey: "existing_key",
		Text:      "hello",
	})

	// Now it should exist.
	exists, err = hb.outbox.RowExists(ctx, "existing_key")
	if err != nil {
		t.Fatalf("RowExists: %v", err)
	}
	if !exists {
		t.Error("expected RowExists to return true for existing key")
	}
}

// TestScanPendingReplies_NoOutbox verifies that ScanPendingReplies is a
// no-op when the outbox is nil (file-only deployments).
func TestScanPendingReplies_NoOutbox(t *testing.T) {
	hb := newHub(t, t.TempDir()+"/test.db")
	hb.h.Outbox = nil // simulate file-only deployment

	ctx := context.Background()
	sessionID := "telegram_nooutbox"

	hb.db.ExecContext(ctx, `
		INSERT INTO chat_pending_replies (session_id, message_id, content, started_at)
		VALUES (?, ?, ?, ?)
	`, sessionID, "msg_005", "hello", time.Now().Add(-5*time.Minute))

	if err := hb.h.ScanPendingReplies(ctx); err != nil {
		t.Fatalf("ScanPendingReplies with nil outbox: %v", err)
	}

	// Pending reply should still be there (no outbox to clear via).
	pending, err := hb.h.Store.ListPendingReplies(ctx)
	if err != nil {
		t.Fatalf("ListPendingReplies: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.SessionID == sessionID {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected pending reply to remain when outbox is nil")
	}
}
