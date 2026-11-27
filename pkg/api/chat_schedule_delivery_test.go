package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/tools"
)

// Tests for scheduled task delivery to a chat session — Phase 2 of
// SCHEDULE_DELIVERY_TARGETS_DESIGN.md.

// TestDeliverToSession_StartsWakeTurn verifies that DeliverToSession starts a
// wake turn in the target session and the model produces an answer.
func TestDeliverToSession_StartsWakeTurn(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "the scheduled task produced 3 files")
	sess := store.GetOrCreate("s_sched")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the nightly report"}, "")

	result := map[string]any{
		"task_id": "task_sched_1",
		"status":  "completed",
		"task":    "nightly report",
	}

	err := h.DeliverToSession(context.Background(), "s_sched", "task_sched_1", "sched_001", result)
	if err != nil {
		t.Fatalf("DeliverToSession failed: %v", err)
	}

	if !waitForAssistantMessage(t, sess, "3 files", 5*time.Second) {
		t.Fatalf("no wake answer in session; messages=%+v", sess.SnapshotMessages())
	}
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("provider was never called — wake turn did not start")
	}

	// The followup_turn event must carry the cron reason.
	found := false
	for _, ev := range sess.EventsAfter(0) {
		if ev.Type == "followup_turn" {
			if strings.Contains(ev.Data, "cron:") {
				found = true
			}
		}
	}
	if !found {
		t.Error("followup_turn event did not carry a cron: reason")
	}
}

// TestCronWake_BypassesDepthCap verifies that a cron-prefixed reason bypasses
// the session-level maxFollowUpDepth cap. Without this, a schedule running
// every 10 minutes would exhaust the session's depth cap (3) in 30 minutes.
func TestCronWake_BypassesDepthCap(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "cron answer")
	sess := store.GetOrCreate("s_cron")
	// Set depth to the cap — a regular follow-up would be suppressed.
	h.followUpDepth.Store("s_cron", maxFollowUpDepth)

	// A cron-prefixed reason must bypass the cap.
	outcome := h.startFollowUpTurnWithTriggerSeed(
		"s_cron", "task-cron", "cron:sched_001:1700000000",
		"[System notice] Scheduled task task-cron has finished.", wakeSeed{})

	if outcome != followUpStarted {
		t.Fatalf("cron wake was suppressed at depth cap; outcome=%v", outcome)
	}

	// Wait for the goroutine to complete before checking provider calls.
	if !waitForAssistantMessage(t, sess, "cron answer", 5*time.Second) {
		t.Fatal("no wake answer in session — cron wake did not produce output")
	}
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("provider was never called — cron wake did not start")
	}

	// The depth counter must NOT have been incremented for cron wakes.
	if v, ok := h.followUpDepth.Load("s_cron"); ok {
		if v.(int) != maxFollowUpDepth {
			t.Errorf("cron wake incremented depth to %d, should stay at %d", v.(int), maxFollowUpDepth)
		}
	}
}

// TestCronWake_RegularFollowUpStillCapped verifies that regular (non-cron)
// follow-ups are still subject to the depth cap after the cron bypass change.
func TestCronWake_RegularFollowUpStillCapped(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "answer")
	store.GetOrCreate("s_reg")
	h.followUpDepth.Store("s_reg", maxFollowUpDepth)

	outcome := h.startFollowUpTurnWithTriggerSeed(
		"s_reg", "task-reg", "task_done",
		"[System notice] Delegated task task-reg has finished.", wakeSeed{})

	if outcome != followUpSuppressed {
		t.Fatalf("regular follow-up at depth cap should be suppressed, got outcome=%v", outcome)
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d time(s) past the depth cap", provider.calls)
	}
}

// TestDeliverToSession_BusySession_QueuesWake verifies that when the session
// is busy (a turn is in flight), the wake is queued for later delivery.
func TestDeliverToSession_BusySession_QueuesWake(t *testing.T) {
	h, store, _ := newFollowUpHandler(t, "first answer")
	store.GetOrCreate("s_busy")

	// Claim the session slot to simulate a busy session.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cancelFuncs.Store("s_busy", context.CancelFunc(cancel))

	result := map[string]any{
		"task_id": "task_busy_1",
		"status":  "completed",
		"task":    "scheduled report",
	}

	err := h.DeliverToSession(context.Background(), "s_busy", "task_busy_1", "sched_002", result)
	if !errors.Is(err, core.ErrSessionBusy) {
		t.Fatalf("DeliverToSession error = %v, want ErrSessionBusy for durable retry", err)
	}

	// The wake should be queued, not dropped.
	v, ok := h.pendingWake.Load("s_busy")
	if !ok {
		t.Fatal("no pending wake queue for busy session")
	}
	q, ok := v.(*wakeQueue)
	if !ok || q == nil {
		t.Fatal("pending wake queue is wrong type")
	}
	entries := q.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 queued wake, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].reason, "cron:") {
		t.Errorf("queued wake reason = %s, should start with cron:", entries[0].reason)
	}
}

// TestDeliverToSession_StrictOwnerMatch is a regression test for
// docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md: multi-user delivery must
// fail closed on either side of an owner mismatch, including an empty session owner.
func TestDeliverToSession_StrictOwnerMatch(t *testing.T) {
	h, sessionStore, _ := newFollowUpHandler(t, "answer")
	h.App = &tools.App{OwnerIsolation: true, Tier: tools.TierPublic}
	for _, tt := range []struct{ sessionOwner, expected string }{
		{"", "alice"},
		{"alice", ""},
		{"alice", "bob"},
	} {
		id := "strict-" + tt.sessionOwner + "-" + tt.expected
		sessionStore.GetOrCreate(id).SetOwnerID(tt.sessionOwner)
		err := h.DeliverToSession(context.Background(), id, "task-"+id, "schedule", map[string]any{
			"status": "completed", "delivery_owner": tt.expected,
		})
		if !errors.Is(err, core.ErrSessionMissing) {
			t.Errorf("session owner %q expected %q: got %v, want ErrSessionMissing", tt.sessionOwner, tt.expected, err)
		}
	}
}

// TestDeliverToSession_LegacyOwnerlessScheduleKeepsLocalCompatibility preserves
// ownerless schedules targeting named local sessions such as Telegram.
func TestDeliverToSession_LegacyOwnerlessScheduleKeepsLocalCompatibility(t *testing.T) {
	h, sessionStore, _ := newFollowUpHandler(t, "local answer")
	sess := sessionStore.GetOrCreate("local-owned")
	sess.SetOwnerID("telegram:42")
	if err := h.DeliverToSession(context.Background(), sess.ID, "task-local", "schedule", map[string]any{
		"status": "completed", "delivery_owner": "",
	}); err != nil {
		t.Fatalf("local ownerless schedule delivery failed: %v", err)
	}
	if !waitForAssistantMessage(t, sess, "local answer", 5*time.Second) {
		t.Fatal("local ownerless schedule delivery did not start a wake turn")
	}
	h.wg.Wait()
}

// TestDeliverToSession_MatchingOwnerAllowed is the positive control for
// docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md: strict comparison must
// still deliver a scheduled result to its named owner.
func TestDeliverToSession_MatchingOwnerAllowed(t *testing.T) {
	h, sessionStore, _ := newFollowUpHandler(t, "owned answer")
	sess := sessionStore.GetOrCreate("owned-delivery")
	sess.SetOwnerID("alice")
	err := h.DeliverToSession(context.Background(), "owned-delivery", "task-owned", "schedule", map[string]any{
		"status": "completed", "delivery_owner": "alice",
	})
	if err != nil {
		t.Fatalf("matching owner delivery failed: %v", err)
	}
	if !waitForAssistantMessage(t, sess, "owned answer", 5*time.Second) {
		t.Fatal("matching owner delivery did not start a wake turn")
	}
	h.wg.Wait()
}

// TestDeliverToSession_InvalidParams verifies that DeliverToSession returns an
// error when required parameters are missing.
func TestDeliverToSession_InvalidParams(t *testing.T) {
	h, _, _ := newFollowUpHandler(t, "answer")

	tests := []struct {
		name      string
		sessionID string
		taskID    string
	}{
		{"empty session", "", "task_1"},
		{"empty task", "sess_1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.DeliverToSession(context.Background(), tt.sessionID, tt.taskID, "sched_1", map[string]any{})
			if err == nil {
				t.Error("expected error for invalid params")
			}
		})
	}
}
