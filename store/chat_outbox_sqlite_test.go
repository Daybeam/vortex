package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newOutboxTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := InitDB(t.TempDir() + "/outbox_test.db")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func enqueueRow(t *testing.T, o *SQLiteChatOutbox, key, text string) bool {
	t.Helper()
	added, err := o.Enqueue(context.Background(), OutboxItem{
		SessionID: "sess-1",
		Kind:      "task_callback",
		DedupeKey: key,
		Text:      text,
		MetaJSON:  `{"task_id":"task_1"}`,
	})
	if err != nil {
		t.Fatalf("Enqueue(%s): %v", key, err)
	}
	return added
}

func outboxStatus(t *testing.T, db *sql.DB, id int64) (status string, attempts int) {
	t.Helper()
	err := db.QueryRow(`SELECT status, attempts FROM chat_outbox WHERE id = ?`, id).Scan(&status, &attempts)
	if err != nil {
		t.Fatalf("read status of %d: %v", id, err)
	}
	return status, attempts
}

// TestChatOutbox_EnqueueIsIdempotentPerDedupeKey covers the producer retry: a
// hub that lost its DB response and enqueues the same notification again must
// not give the user two copies.
func TestChatOutbox_EnqueueIsIdempotentPerDedupeKey(t *testing.T) {
	o := NewSQLiteChatOutbox(newOutboxTestDB(t))

	if !enqueueRow(t, o, "task_callback|task_1", "first") {
		t.Fatal("first enqueue reported no row written")
	}
	if enqueueRow(t, o, "task_callback|task_1", "retry") {
		t.Error("retry with the same dedupe_key wrote a second row")
	}

	var count int
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM chat_outbox`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

// TestChatOutbox_ClaimHandsEachRowToOneHolder is the multi-hub guarantee: five
// hubs on one host -- or N pods on k3s behind a shared queue -- poll the same
// table, and a notification still reaches the user once. There is no mutex in
// the claim, so the invariant has to come from the guarded UPDATE alone.
func TestChatOutbox_ClaimHandsEachRowToOneHolder(t *testing.T) {
	const (
		hubs  = 5
		rows  = 30
		limit = 10
	)
	db := newOutboxTestDB(t)
	o := NewSQLiteChatOutbox(db)
	for i := 0; i < rows; i++ {
		enqueueRow(t, o, fmt.Sprintf("task_callback|task_%d", i), "body")
	}

	ctx := context.Background()
	var mu sync.Mutex
	claimed := make(map[int64]int)

	// Every hub keeps claiming until the queue runs dry for it; a hub that
	// loses every race in a pass simply comes back for another.
	for round := 0; round < 200; round++ {
		var wg sync.WaitGroup
		for h := 0; h < hubs; h++ {
			wg.Add(1)
			go func(hub int) {
				defer wg.Done()
				items, err := o.ClaimPending(ctx, fmt.Sprintf("hub-%d", hub), limit, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				for _, it := range items {
					claimed[it.ID]++
				}
				mu.Unlock()
				for _, it := range items {
					if ok, err := o.MarkDelivered(ctx, it.ID, fmt.Sprintf("hub-%d", hub)); err != nil || !ok {
						t.Errorf("mark delivered %d: ok=%v err=%v", it.ID, ok, err)
					}
				}
			}(h)
		}
		wg.Wait()

		mu.Lock()
		done := len(claimed)
		mu.Unlock()
		if done == rows {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(claimed) != rows {
		t.Fatalf("expected all %d rows claimed, got %d", rows, len(claimed))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("row %d was handed to %d holders, want 1 (double-send)", id, n)
		}
	}

	var undelivered int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chat_outbox WHERE status != 'delivered'`).Scan(&undelivered); err != nil {
		t.Fatalf("count undelivered: %v", err)
	}
	if undelivered != 0 {
		t.Errorf("%d rows never reached 'delivered'", undelivered)
	}
}

// TestChatOutbox_StaleHolderCannotConfirmAfterTakeover: a hub that lost its
// lease -- it hung during a send, or was descheduled -- must not be able to
// mark a row delivered that another hub already re-sent. claim_holder is the
// fence, per HUB_HA_PRIMARY_STANDBY_DESIGN.md invariant 2 (fencing beats
// self-belief).
func TestChatOutbox_StaleHolderCannotConfirmAfterTakeover(t *testing.T) {
	db := newOutboxTestDB(t)
	o := NewSQLiteChatOutbox(db)
	enqueueRow(t, o, "decision_needed|task_1|dec_1", "approve?")

	ctx := context.Background()
	first, err := o.ClaimPending(ctx, "hub-A", 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("hub-A claim: items=%d err=%v", len(first), err)
	}
	id := first[0].ID

	// Simulate hub-A's lease lapsing without it ever confirming.
	if _, err := db.Exec(`UPDATE chat_outbox SET claim_expires_at = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	second, err := o.ClaimPending(ctx, "hub-B", 1, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("hub-B takeover claim: items=%d err=%v", len(second), err)
	}
	if second[0].ID != id {
		t.Fatalf("hub-B claimed a different row: %d want %d", second[0].ID, id)
	}

	if ok, err := o.MarkDelivered(ctx, id, "hub-A"); err != nil {
		t.Fatalf("stale MarkDelivered: %v", err)
	} else if ok {
		t.Error("stale holder hub-A was able to confirm a row hub-B now owns")
	}
	if err := o.MarkFailed(ctx, id, "hub-A", "stale"); err != nil {
		t.Fatalf("stale MarkFailed: %v", err)
	}
	// A stale failure report must not release hub-B's live claim either.
	var holder string
	if err := db.QueryRow(`SELECT claim_holder FROM chat_outbox WHERE id = ?`, id).Scan(&holder); err != nil {
		t.Fatalf("read holder: %v", err)
	}
	if holder != "hub-B" {
		t.Errorf("stale MarkFailed moved the row to holder %q, want hub-B still holding it", holder)
	}

	if ok, err := o.MarkDelivered(ctx, id, "hub-B"); err != nil || !ok {
		t.Fatalf("current holder hub-B should confirm: ok=%v err=%v", ok, err)
	}
	if status, _ := outboxStatus(t, db, id); status != "delivered" {
		t.Errorf("status = %q, want delivered", status)
	}
}

// TestChatOutbox_MarkFailedRetriesThenGivesUp keeps one permanently undeliverable
// notification from spinning through the queue forever -- a revoked bot token
// used to make every send fail.
func TestChatOutbox_MarkFailedRetriesThenGivesUp(t *testing.T) {
	db := newOutboxTestDB(t)
	o := NewSQLiteChatOutbox(db)
	enqueueRow(t, o, "task_callback|task_9", "gone")

	ctx := context.Background()
	var lastID int64
	for pass := 1; pass <= maxOutboxAttempts; pass++ {
		items, err := o.ClaimPending(ctx, "hub-A", 5, time.Minute)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(items) != 1 {
			t.Fatalf("pass %d: expected the row to still be claimable, got %d items", pass, len(items))
		}
		lastID = items[0].ID
		if items[0].Attempts != pass {
			t.Errorf("pass %d: attempts = %d, want %d", pass, items[0].Attempts, pass)
		}
		if err := o.MarkFailed(ctx, items[0].ID, "hub-A", "401 Unauthorized"); err != nil {
			t.Fatalf("pass %d MarkFailed: %v", pass, err)
		}
	}

	if status, attempts := outboxStatus(t, db, lastID); status != "dropped" || attempts != maxOutboxAttempts {
		t.Errorf("after the cap: status=%q attempts=%d, want dropped/%d", status, attempts, maxOutboxAttempts)
	}
	items, err := o.ClaimPending(ctx, "hub-A", 5, time.Minute)
	if err != nil || len(items) != 0 {
		t.Errorf("a dropped row must not be claimable again: items=%d err=%v", len(items), err)
	}
	var reason string
	if err := db.QueryRow(`SELECT last_error FROM chat_outbox WHERE id = ?`, lastID).Scan(&reason); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if reason != "401 Unauthorized" {
		t.Errorf("last_error = %q, want the failure reason recorded for the operator", reason)
	}
}

// TestChatOutbox_PurgeKeepsPendingAndRecentTerminalRows bounds the table: only
// finished rows older than the retention bound go, so a delivery nobody has
// claimed yet can never be collected.
func TestChatOutbox_PurgeKeepsPendingAndRecentTerminalRows(t *testing.T) {
	db := newOutboxTestDB(t)
	o := NewSQLiteChatOutbox(db)
	ctx := context.Background()

	enqueueRow(t, o, "task_callback|old", "old")
	enqueueRow(t, o, "task_callback|fresh", "fresh")
	enqueueRow(t, o, "task_callback|pending", "never claimed")

	items, err := o.ClaimPending(ctx, "hub-A", 2, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected the two oldest rows, got %d", len(items))
	}
	for _, it := range items {
		if _, err := o.MarkDelivered(ctx, it.ID, "hub-A"); err != nil {
			t.Fatalf("deliver %s: %v", it.DedupeKey, err)
		}
	}

	// Age the delivered-old and the never-claimed rows past the retention
	// bound, leaving the freshly delivered row alone: how old a row is must not
	// decide whether an undelivered notification survives, only whether a
	// finished one is still worth keeping.
	if _, err := db.Exec(`UPDATE chat_outbox SET created_at = ? WHERE dedupe_key != 'task_callback|fresh'`,
		time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := o.Purge(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("Purge removed %d rows, want exactly 1", n)
	}

	var remaining []string
	rows, err := db.Query(`SELECT dedupe_key FROM chat_outbox ORDER BY id`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, k)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining rows = %v, want the fresh delivered one and the pending one", remaining)
	}
	for _, k := range remaining {
		if k == "task_callback|old" {
			t.Error("the stale delivered row survived Purge")
		}
	}
}
