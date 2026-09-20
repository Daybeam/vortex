package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

func TestSQLiteScheduleBackend(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "schedule_test")
	defer os.RemoveAll(tmpDir)

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
	list, _ = backend.LoadAll(ctx)
	if len(list) != 0 {
		t.Errorf("Delete failed to remove item")
	}
}
