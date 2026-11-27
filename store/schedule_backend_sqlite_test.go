package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

func TestSQLiteScheduleBackendPreservesDeliveryAndRunLedger(t *testing.T) {
	// Regression (SCHEDULE_DELIVERY_TARGETS_DESIGN §4/P0): the legacy schedule
	// columns silently discarded Deliver, Type, MaxRuns and failure counters.
	db, err := InitDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := NewSQLiteScheduleBackend(db)
	ctx := context.Background()
	now := time.Now().Round(time.Second)
	next := now.Add(time.Hour)
	s := &schemas.Schedule{
		ID: "sched_delivery", Name: "delivery", Type: schemas.ScheduleInterval,
		Interval: "1h", Status: schemas.ScheduleActive, MaxRuns: 7, RunCount: 3,
		LastRun: &now, NextRun: next, OnFailure: schemas.FailureRetryNextWindow,
		ConsecutiveFailures: 2,
		Deliver: schemas.DeliverTarget{Mode: "session_current", SessionID: "telegram_42", OnMissing: "inbox"},
		TaskInputs: []schemas.StepInput{{Task: "work"}}, CreatedAt: now,
	}
	if err := backend.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	list, err := backend.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d schedules", len(list))
	}
	got := list[0]
	if got.Deliver != s.Deliver || got.Type != s.Type || got.Interval != s.Interval ||
		got.MaxRuns != s.MaxRuns || got.RunCount != s.RunCount || got.OnFailure != s.OnFailure ||
		got.ConsecutiveFailures != s.ConsecutiveFailures || !got.NextRun.Equal(next) {
		t.Fatalf("schedule snapshot mismatch: got %+v", got)
	}
}

func TestSQLiteScheduleBackendLoadsLegacyRowsAfterMigration(t *testing.T) {
	// Regression (SCHEDULE_DELIVERY_TARGETS_DESIGN §4/P0): existing databases
	// must keep loading rows created before schedule_json existed.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`ALTER TABLE schedules RENAME TO schedules_new`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE schedules (id TEXT PRIMARY KEY, name TEXT, cron TEXT, task_request_json TEXT, last_run DATETIME, next_run DATETIME, is_enabled BOOLEAN DEFAULT TRUE, created_at DATETIME)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO schedules (id, name, cron, task_request_json, next_run, is_enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"sched_old", "old", "0 9 * * *", `[{"task":"legacy"}]`, time.Now(), true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = InitDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := NewSQLiteScheduleBackend(db).LoadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "sched_old" || len(list[0].TaskInputs) != 1 {
		t.Fatalf("legacy schedule not preserved: %+v", list)
	}
}

func TestSQLiteScheduleBackend(t *testing.T) {
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteScheduleBackend(db)
	ctx := context.Background()

	s := &schemas.Schedule{
		ID:       "sched_1",
		Name:     "Daily Backup",
		Type:     schemas.ScheduleCron,
		Status:   schemas.ScheduleActive,
		CronExpr: "0 0 * * *",
		TaskInputs: []schemas.StepInput{
			{Task: "backup all data"},
		},
		CreatedAt: time.Now().Round(time.Second),
	}

	// 1. Save
	if err := backend.Save(ctx, s); err != nil {
		t.Errorf("Save failed: %v", err)
	}

	// 2. Update LastRun
	now := time.Now().Round(time.Second)
	s.LastRun = &now
	if err := backend.Save(ctx, s); err != nil {
		t.Errorf("Update failed: %v", err)
	}

	// 3. LoadAll
	list, err := backend.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != s.ID {
		t.Errorf("LoadAll mismatch: got %v", list)
	}
	if list[0].LastRun == nil || !list[0].LastRun.Equal(now) {
		t.Errorf("LastRun mismatch: got %v, want %v", list[0].LastRun, now)
	}

	// 4. Delete
	if err := backend.Delete(ctx, s.ID); err != nil {
		t.Errorf("Delete failed: %v", err)
	}
	// fixes audit T-C19: original swallowed LoadAll error — if LoadAll
	// failed, list was nil, len(nil)==0, and the assertion passed trivially.
	list, err = backend.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll after delete failed: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("Delete failed to remove item")
	}
}
