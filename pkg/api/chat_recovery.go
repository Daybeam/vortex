package api

// Startup scan for interrupted chat turns.
//
// When a process dies between a user message and its reply, the
// chat_pending_replies table retains the mark set on turn start. On the
// next startup, this scan finds those marks and either:
//   - clears them (if the reply was already enqueued to the outbox before
//     the crash — the pump will deliver it), or
//   - enqueues a chat_interrupted notice so the user is at least told
//     "your last message was interrupted" rather than receiving silence.
//
// See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md §3.6.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/daybeam/vortex/store"
)

// pendingReplyAgeThreshold is the minimum age a pending reply must be
// before the scan treats it as interrupted. A pending reply younger than
// this may belong to a turn that was in progress when the process was
// gracefully restarted (not crashed), so notifying the user would be a
// false alarm.
const pendingReplyAgeThreshold = 2 * time.Minute

// ScanPendingReplies checks the DB for chat turns that were interrupted
// by a crash and either clears their pending mark (if the reply was
// already enqueued) or enqueues a chat_interrupted notification.
//
// Safe to call on startup. No-op when the session store has no DB backend
// or the outbox is nil (file-only deployments).
func (h *ChatHandler) ScanPendingReplies(ctx context.Context) error {
	if h.Outbox == nil {
		return nil // no outbox → no crash recovery path
	}

	pending, err := h.Store.ListPendingReplies(ctx)
	if err != nil {
		return fmt.Errorf("list pending replies: %w", err)
	}

	now := time.Now()
	for _, p := range pending {
		// Skip recent pending replies — the turn may still be in progress
		// if the process was gracefully restarted (not crashed).
		if now.Sub(p.StartedAt) < pendingReplyAgeThreshold {
			continue
		}

		dedupeKey := "chat_reply:" + p.SessionID + ":" + p.MessageID

		// If the chat_reply was already enqueued, the pump will deliver
		// it. Just clear the pending mark.
		exists, err := h.Outbox.RowExists(ctx, dedupeKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[chat] WARN: RowExists failed for %s: %v\n", dedupeKey, err)
			continue
		}

		if !exists {
			// The turn was interrupted before the reply was enqueued.
			// Notify the user so they at least know to resend.
			interruptKey := "chat_interrupted:" + p.SessionID + ":" + p.MessageID
			text := fmt.Sprintf("⚠️ Your previous message (ID %s) was interrupted by a process restart. Please resend it.", p.MessageID)
			if added, err := h.Outbox.Enqueue(ctx, store.OutboxItem{
				SessionID: p.SessionID,
				Kind:      kindChatInterrupt,
				DedupeKey: interruptKey,
				Text:      text,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "[chat] WARN: failed to enqueue chat_interrupted for %s: %v\n", p.SessionID, err)
			} else if added {
				h.logOutbox(eventNotificationQueued, map[string]any{
					"kind": kindChatInterrupt, "dedupe_key": interruptKey,
					"session_id": p.SessionID,
				})
			}
		}

		// Clear the pending mark regardless — the turn is either
		// completed (reply enqueued) or notified (interrupted).
		if err := h.Store.ClearPendingReply(ctx, p.SessionID); err != nil {
			fmt.Fprintf(os.Stderr, "[chat] WARN: ClearPendingReply failed for %s: %v\n", p.SessionID, err)
		}
	}

	return nil
}
