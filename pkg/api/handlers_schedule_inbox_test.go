package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — inbox scope
// must be applied before pagination, with no anonymous claim on legacy rows.
func TestScheduleInboxHandler_MultiUserScopeBeforeLimit(t *testing.T) {
	// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — anonymous
	// readers could see ownerless rows, and foreign rows consumed the page limit.
	hb := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	outbox := store.NewSQLiteChatOutbox(hb.db)
	manager := core.NewScheduleManager(hb.engine, nil, nil, hb.logger)
	manager.SetDeliveryOutbox(outbox)
	for i, owner := range []string{"bob", "bob", "alice", ""} {
		payload, err := json.Marshal(map[string]any{
			"delivery": schemas.ScheduleDelivery{ScheduleID: "sched_" + owner, OwnerID: owner,
				Target: schemas.DeliverTarget{Mode: "inbox", SessionID: "s_" + owner}},
			"task_id": "task_" + owner, "result": map[string]any{"status": "completed"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := outbox.Enqueue(context.Background(), store.OutboxItem{
			OwnerID: owner, Kind: "schedule_inbox", DedupeKey: fmt.Sprintf("inbox|%s|%d", owner, i), Text: string(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := &APIHandler{App: &tools.App{OwnerIsolation: true, Tier: tools.TierPublic, Schedule: manager}}
	for _, tc := range []struct {
		name, owner, tier string
		want              int
	}{
		{"owner before limit", "alice", tools.TierPublic, 1},
		{"anonymous cannot claim legacy", "", tools.TierPublic, 0},
		{"admin positive control", "", tools.TierAdmin, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/schedules/inbox?limit=1", nil)
			req = req.WithContext(context.WithValue(context.Background(), tools.AuthOwnerKey, tc.owner))
			req = req.WithContext(context.WithValue(req.Context(), tools.AuthTierKey, tc.tier))
			rec := httptest.NewRecorder()
			h.ScheduleInboxHandler(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response struct {
				Items []core.ScheduleInboxEntry `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Items) != tc.want {
				t.Fatalf("items=%+v, want %d", response.Items, tc.want)
			}
			if tc.want == 1 && response.Items[0].OwnerID != map[string]string{"owner before limit": "alice", "admin positive control": "bob"}[tc.name] {
				t.Fatalf("wrong owner entry: %+v", response.Items)
			}
		})
	}
}

// Regression (SCHEDULE_DELIVERY_TARGETS_DESIGN §4/P0): a missing session must
// fall back to a readable durable inbox rather than an operator-only log.
func TestScheduleInboxHandler_ReturnsOwnedFallback(t *testing.T) {
	hb := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	outbox := store.NewSQLiteChatOutbox(hb.db)
	manager := core.NewScheduleManager(hb.engine, nil, nil, hb.logger)
	manager.SetDeliveryOutbox(outbox)
	payload, err := json.Marshal(map[string]any{
		"delivery": schemas.ScheduleDelivery{ScheduleID: "sched_inbox", OwnerID: "alice",
			Target: schemas.DeliverTarget{Mode: "inbox", SessionID: "s_alice"}},
		"task_id": "task_inbox", "result": map[string]any{"status": "completed"}, "reason": "session missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Enqueue(context.Background(), store.OutboxItem{
		OwnerID: "alice", Kind: "schedule_inbox", DedupeKey: "schedule_inbox|alice", Text: string(payload),
	}); err != nil {
		t.Fatal(err)
	}
	handler := &APIHandler{App: &tools.App{Schedule: manager}}
	req := httptest.NewRequest("GET", "/api/schedules/inbox", nil)
	req = req.WithContext(context.WithValue(req.Context(), tools.AuthOwnerKey, "alice"))
	rec := httptest.NewRecorder()
	handler.ScheduleInboxHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Items []core.ScheduleInboxEntry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].TaskID != "task_inbox" {
		t.Fatalf("inbox response = %+v", response)
	}
}
