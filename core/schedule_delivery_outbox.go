package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

const (
	scheduleWebhookKind = "schedule_webhook"
	scheduleSessionKind = "schedule_session"
	scheduleInboxKind   = "schedule_inbox"

	// deliveryOpTimeout caps each delivery-loop DB operation and session
	// delivery. The webhook HTTP call has its own shorter timeout
	// (webhookDeliveryTimeout in schedule_delivery.go); this covers the
	// session delivery path and all outbox DB writes that would otherwise
	// hang indefinitely on context.Background().
	deliveryOpTimeout = 30 * time.Second
)

type scheduleDeliveryPayload struct {
	Delivery schemas.ScheduleDelivery `json:"delivery"`
	TaskID   string                   `json:"task_id"`
	Result   map[string]any           `json:"result"`
	Reason   string                   `json:"reason,omitempty"`
}

func scheduleDeliveryFor(s *schemas.Schedule) schemas.ScheduleDelivery {
	return schemas.ScheduleDelivery{ScheduleID: s.ID, Name: s.Name, Target: s.Deliver, OwnerID: s.OwnerID}
}

func scheduleDeliveryKind(target schemas.DeliverTarget) string {
	if target.Mode == "session_current" {
		return scheduleSessionKind
	}
	return scheduleWebhookKind
}

func (sm *ScheduleManager) SetDeliveryOutbox(outbox store.IChatOutbox) {
	sm.deliveryOutbox = outbox
}

func (sm *ScheduleManager) QueueCompletedDelivery(delivery schemas.ScheduleDelivery, taskID string, result map[string]any) error {
	if delivery.ScheduleID == "" || taskID == "" || delivery.Target.Mode == "" {
		return fmt.Errorf("schedule delivery: incomplete task delivery descriptor")
	}
	if sm.deliveryOutbox == nil {
		return fmt.Errorf("schedule delivery: durable outbox is not wired")
	}
	payload := scheduleDeliveryPayload{Delivery: delivery, TaskID: taskID, Result: make(map[string]any, len(result)+2)}
	for key, value := range result {
		payload.Result[key] = value
	}
	payload.Result["task_id"] = taskID
	if delivery.OwnerID != "" {
		payload.Result["delivery_owner"] = delivery.OwnerID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("schedule delivery: encode payload: %w", err)
	}
	kind := scheduleDeliveryKind(delivery.Target)
	ctx, cancel := context.WithTimeout(context.Background(), deliveryOpTimeout)
	defer cancel()
	_, err = sm.deliveryOutbox.Enqueue(ctx, store.OutboxItem{
		SessionID: delivery.Target.SessionID,
		OwnerID:   delivery.OwnerID,
		Kind:      kind,
		DedupeKey: fmt.Sprintf("schedule_delivery|%s|%s|%s", delivery.ScheduleID, taskID, kind),
		Text:      string(encoded),
	})
	return err
}

func (sm *ScheduleManager) queueFallback(delivery schemas.ScheduleDelivery, taskID string, result map[string]any, reason string) error {
	target := delivery.Target
	if target.OnMissing == "webhook" {
		delivery.Target = schemas.DeliverTarget{Mode: "webhook", WebhookURL: target.WebhookURL}
		return sm.QueueCompletedDelivery(delivery, taskID, result)
	}
	if target.OnMissing != "inbox" {
		return fmt.Errorf("schedule delivery: no valid on_missing fallback")
	}
	if sm.deliveryOutbox == nil {
		return fmt.Errorf("schedule delivery: durable inbox is not wired")
	}
	delivery.Target = schemas.DeliverTarget{Mode: "inbox", SessionID: target.SessionID}
	payload := scheduleDeliveryPayload{Delivery: delivery, TaskID: taskID, Result: result, Reason: reason}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deliveryOpTimeout)
	defer cancel()
	_, err = sm.deliveryOutbox.Enqueue(ctx, store.OutboxItem{
		SessionID: target.SessionID,
		OwnerID:   delivery.OwnerID,
		Kind:      scheduleInboxKind,
		DedupeKey: fmt.Sprintf("schedule_inbox|%s|%s", delivery.ScheduleID, taskID),
		Text:      string(encoded),
	})
	return err
}

type ScheduleInboxEntry struct {
	ID         int64          `json:"id"`
	ScheduleID string         `json:"schedule_id"`
	TaskID     string         `json:"task_id"`
	Name       string         `json:"name,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	OwnerID    string         `json:"owner_id,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Result     map[string]any `json:"result"`
	CreatedAt  time.Time      `json:"created_at"`
}

func (sm *ScheduleManager) ListPendingInbox(ctx context.Context, ownerID string, limit int) ([]ScheduleInboxEntry, error) {
	return sm.listPendingInbox(ctx, ownerID, false, limit)
}

// ListPendingInboxForScope lets an administrator or local operator read the full
// inbox; ordinary multi-user callers are restricted before the database LIMIT.
func (sm *ScheduleManager) ListPendingInboxForScope(ctx context.Context, ownerID string, includeAll bool, limit int) ([]ScheduleInboxEntry, error) {
	return sm.listPendingInbox(ctx, ownerID, includeAll, limit)
}

func (sm *ScheduleManager) listPendingInbox(ctx context.Context, ownerID string, includeAll bool, limit int) ([]ScheduleInboxEntry, error) {
	if !includeAll && ownerID == "" {
		return []ScheduleInboxEntry{}, nil
	}
	var items []store.OutboxItem
	if reader, ok := sm.deliveryOutbox.(interface {
		ListPendingKindsScoped(context.Context, []string, string, bool, int) ([]store.OutboxItem, error)
	}); ok {
		var err error
		items, err = reader.ListPendingKindsScoped(ctx, []string{scheduleInboxKind}, ownerID, includeAll, limit)
		if err != nil {
			return nil, err
		}
	} else if !includeAll {
		return nil, fmt.Errorf("schedule inbox: owner-scoped durable reader is not wired")
	} else if reader, ok := sm.deliveryOutbox.(interface {
		ListPendingKinds(context.Context, []string, int) ([]store.OutboxItem, error)
	}); ok {
		var err error
		items, err = reader.ListPendingKinds(ctx, []string{scheduleInboxKind}, limit)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("schedule inbox: durable reader is not wired")
	}
	entries := make([]ScheduleInboxEntry, 0, len(items))
	for _, item := range items {
		var payload scheduleDeliveryPayload
		if err := json.Unmarshal([]byte(item.Text), &payload); err != nil {
			return nil, fmt.Errorf("schedule inbox %d: decode payload: %w", item.ID, err)
		}
		if !includeAll && (item.OwnerID != ownerID || payload.Delivery.OwnerID != ownerID) {
			continue
		}
		entries = append(entries, ScheduleInboxEntry{
			ID: item.ID, ScheduleID: payload.Delivery.ScheduleID, TaskID: payload.TaskID,
			Name: payload.Delivery.Name, SessionID: payload.Delivery.Target.SessionID,
			OwnerID: payload.Delivery.OwnerID, Reason: payload.Reason,
			Result: payload.Result, CreatedAt: item.CreatedAt,
		})
	}
	return entries, nil
}

func (sm *ScheduleManager) deliveryLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		sm.deliverQueuedResults()
		select {
		case <-sm.stopChan:
			return
		case <-ticker.C:
		}
	}
}

func (sm *ScheduleManager) deliverQueuedResults() {
	if sm.deliveryOutbox == nil {
		return
	}
	holder, _ := os.Hostname()
	claimCtx, claimCancel := context.WithTimeout(context.Background(), deliveryOpTimeout)
	defer claimCancel()
	items, err := sm.deliveryOutbox.ClaimPendingKinds(claimCtx, holder, 10, 30*time.Second,
		[]string{scheduleWebhookKind, scheduleSessionKind})
	if err != nil {
		sm.logger.Log("schedule_delivery_claim_failed", "", "", map[string]any{"error": err.Error()})
		return
	}
	for _, item := range items {
		var payload scheduleDeliveryPayload
		if err := json.Unmarshal([]byte(item.Text), &payload); err != nil {
			sm.logOutboxDeliveryFailure(holder, item, fmt.Errorf("decode payload: %w", err))
			continue
		}
		s := &schemas.Schedule{ID: payload.Delivery.ScheduleID, Name: payload.Delivery.Name, Deliver: payload.Delivery.Target, OwnerID: payload.Delivery.OwnerID}
		payload.Result["task_id"] = payload.TaskID
		deliverCtx, deliverCancel := context.WithTimeout(context.Background(), deliveryOpTimeout)
		if err := sm.deliverResult(deliverCtx, s, payload.Result); err != nil {
			deliverCancel()
			sm.logOutboxDeliveryFailure(holder, item, err)
			continue
		}
		deliverCancel()
		markCtx, markCancel := context.WithTimeout(context.Background(), deliveryOpTimeout)
		confirmed, err := sm.deliveryOutbox.MarkDelivered(markCtx, item.ID, holder)
		markCancel()
		if err != nil || !confirmed {
			sm.logger.Log("schedule_delivery_unconfirmed", "", "", map[string]any{
				"schedule_id": s.ID, "task_id": payload.TaskID, "confirmed": confirmed, "error": deliveryError(err),
			})
		}
	}
}

func (sm *ScheduleManager) logOutboxDeliveryFailure(holder string, item store.OutboxItem, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryOpTimeout)
	defer cancel()
	if err := sm.deliveryOutbox.MarkFailed(ctx, item.ID, holder, cause.Error()); err != nil {
		sm.logger.Log(obs.EventStepFailed, "", "", map[string]any{
			"schedule_id": item.DedupeKey, "kind": item.Kind, "error": cause.Error(),
			"delivery": "outbox_retry_release_failed", "retry_error": err.Error(),
		})
		return
	}
	sm.logger.Log(obs.EventStepFailed, "", "", map[string]any{
		"schedule_id": item.DedupeKey, "kind": item.Kind, "error": cause.Error(), "delivery": "outbox_retry",
	})
}

func deliveryError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
