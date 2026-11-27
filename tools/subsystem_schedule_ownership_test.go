package tools

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — schedule
// list and mutations were global even though each row carried an owner.
func TestScheduleActions_OwnerIsolationAndLocalCompatibility(t *testing.T) {
	app := &App{Tier: TierPublic, OwnerIsolation: true, SStore: store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)}
	registerScheduleSubsystem(app)
	for _, sch := range []*schemas.Schedule{
		{ID: "alice", OwnerID: "alice", Name: "alice", Type: schemas.ScheduleInterval, Interval: "1h", Status: schemas.ScheduleActive, NextRun: time.Now().Add(time.Hour)},
		{ID: "bob", OwnerID: "bob", Name: "bob", Type: schemas.ScheduleInterval, Interval: "1h", Status: schemas.ScheduleActive, NextRun: time.Now().Add(time.Hour)},
		{ID: "legacy", Name: "legacy", Type: schemas.ScheduleInterval, Interval: "1h", Status: schemas.ScheduleActive, NextRun: time.Now().Add(time.Hour)},
	} {
		if err := app.SStore.Set(sch); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)
	ctx = context.WithValue(ctx, AuthOwnerKey, "alice")

	listed, err := registry.Invoke(ctx, app, "schedule", "list", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := listed.([]*schemas.Schedule)
	if len(rows) != 1 || rows[0].ID != "alice" {
		t.Fatalf("alice list = %+v, want only alice", rows)
	}
	for _, action := range []string{"pause", "resume", "delete"} {
		if _, err := registry.Invoke(ctx, app, "schedule", action, map[string]any{"schedule_id": "bob"}); err == nil {
			t.Errorf("cross-owner %s succeeded", action)
		}
		if app.SStore.Get("bob") == nil {
			t.Errorf("cross-owner %s deleted bob's schedule", action)
		}
		if _, err := registry.Invoke(ctx, app, "schedule", action, map[string]any{"schedule_id": "legacy"}); err == nil {
			t.Errorf("multi-user %s claimed an ownerless schedule", action)
		}
	}

	created, err := registry.Invoke(ctx, app, "schedule", "create", map[string]any{
		"name": "owned", "type": "interval", "interval": "1h", "owner_id": "bob",
		"task_inputs": []map[string]any{{"task": "work", "submitter_is_admin": true}},
	})
	if err != nil {
		t.Fatalf("owner create: %v", err)
	}
	id := created.(map[string]any)["schedule_id"].(string)
	got := app.SStore.Get(id)
	if got.OwnerID != "alice" || got.SubmitterIsAdmin {
		t.Fatalf("created schedule forged owner/admin: %+v", got)
	}
	blank := context.WithValue(context.Background(), AuthTierKey, TierPublic)
	if _, err := registry.Invoke(blank, app, "schedule", "create", map[string]any{
		"name": "anonymous", "type": "interval", "interval": "1h", "task_inputs": []map[string]any{{"task": "work"}},
	}); err == nil {
		t.Fatal("multi-user public caller created an ownerless schedule")
	}

	admin := context.WithValue(context.Background(), AuthTierKey, TierAdmin)
	listed, err = registry.Invoke(admin, app, "schedule", "list", nil)
	if err != nil || len(listed.([]*schemas.Schedule)) != 4 {
		t.Fatalf("admin positive control: list=%v err=%v", listed, err)
	}

	app.OwnerIsolation = false
	listed, err = registry.Invoke(blank, app, "schedule", "list", nil)
	if err != nil || len(listed.([]*schemas.Schedule)) != 4 {
		t.Fatalf("local positive control: list=%v err=%v", listed, err)
	}
	if _, err := registry.Invoke(blank, app, "schedule", "pause", map[string]any{"schedule_id": "legacy"}); err != nil {
		t.Fatalf("local ownerless pause: %v", err)
	}
}
