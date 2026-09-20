package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

func newTestDB(t *testing.T) (*Store, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "compactor_test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s, err := NewStore(t.TempDir(), t.TempDir(), "", &config.SystemSettings{}, db)
	if err != nil {
		db.Close()
		t.Fatalf("NewStore: %v", err)
	}
	return s, func() { db.Close() }
}

func TestNewStorageCompactorFromSettings_HeadlessDefaults(t *testing.T) {
	c := NewStorageCompactorFromSettings(nil, nil)
	if c.config.TaskHistoryDays != 30 {
		t.Errorf("TaskHistoryDays default = %d, want 30", c.config.TaskHistoryDays)
	}
	if c.config.StagingTTLHours != 168 {
		t.Errorf("StagingTTLHours default = %d, want 168", c.config.StagingTTLHours)
	}
	if c.config.MaxCompletedTasks != 1000 {
		t.Errorf("MaxCompletedTasks default = %d, want 1000", c.config.MaxCompletedTasks)
	}
	if c.config.MaxPromotionAuditLogs != 1000 {
		t.Errorf("MaxPromotionAuditLogs default = %d, want 1000", c.config.MaxPromotionAuditLogs)
	}
	if c.config.AutoVacuumIntervalDays != 7 {
		t.Errorf("AutoVacuumIntervalDays default = %d, want 7", c.config.AutoVacuumIntervalDays)
	}
}

func TestNewStorageCompactorFromSettings_RespectsExplicitValues(t *testing.T) {
	sys := &config.SystemSettings{
		Retention: config.RetentionSettings{
			TaskHistoryDays:        7,
			MaxCompletedTasks:      100,
			MaxPromotionAuditLogs:  50,
			AutoVacuumIntervalDays: 3,
			VacuumOnStartup:        true,
		},
	}
	c := NewStorageCompactorFromSettings(nil, sys)
	if c.config.TaskHistoryDays != 7 {
		t.Errorf("TaskHistoryDays = %d, want 7", c.config.TaskHistoryDays)
	}
	if c.config.MaxCompletedTasks != 100 {
		t.Errorf("MaxCompletedTasks = %d, want 100", c.config.MaxCompletedTasks)
	}
	if c.config.MaxPromotionAuditLogs != 50 {
		t.Errorf("MaxPromotionAuditLogs = %d, want 50", c.config.MaxPromotionAuditLogs)
	}
	if !c.config.VacuumOnStartup {
		t.Error("VacuumOnStartup = false, want true")
	}
}

func TestRunCompaction_PruneOldTasksByDays(t *testing.T) {
	s, cleanup := newTestDB(t)
	defer cleanup()
	db := s.DB

	old := time.Now().AddDate(0, 0, -40).Format("2006-01-02 15:04:05")
	_, err := db.Exec(`INSERT INTO tasks (task_id, status, updated_at) VALUES ('old_task', 'completed', ?)`, old)
	if err != nil {
		t.Fatalf("insert old task: %v", err)
	}
	_, err = db.Exec(`INSERT INTO tasks (task_id, status) VALUES ('fresh_task', 'completed')`)
	if err != nil {
		t.Fatalf("insert fresh task: %v", err)
	}
	_, err = db.Exec(`INSERT INTO task_steps (task_id, step_id) VALUES ('old_task', 's1')`)
	if err != nil {
		t.Fatalf("insert old step: %v", err)
	}

	c := NewStorageCompactor(db, RetentionConfig{TaskHistoryDays: 30, MaxCompletedTasks: 1000})
	if err := c.RunCompaction(context.Background()); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_id = 'old_task'").Scan(&count)
	if count != 0 {
		t.Error("old task should have been pruned")
	}
	db.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_id = 'fresh_task'").Scan(&count)
	if count != 1 {
		t.Error("fresh task should remain")
	}
	db.QueryRow("SELECT COUNT(*) FROM task_steps WHERE task_id = 'old_task'").Scan(&count)
	if count != 0 {
		t.Error("orphaned task_steps should have been cascade-deleted")
	}
}

func TestRunCompaction_FIFOPruneExcessTasks(t *testing.T) {
	s, cleanup := newTestDB(t)
	defer cleanup()
	db := s.DB

	for i := 0; i < 5; i++ {
		ts := time.Now().Add(-time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		_, err := db.Exec(`INSERT INTO tasks (task_id, status, updated_at) VALUES (?, 'completed', ?)`, "task_"+itoa(i), ts)
		if err != nil {
			t.Fatalf("insert task %d: %v", i, err)
		}
	}

	c := NewStorageCompactor(db, RetentionConfig{TaskHistoryDays: 0, MaxCompletedTasks: 2})
	if err := c.RunCompaction(context.Background()); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM tasks WHERE status = 'completed'").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 tasks after FIFO prune, got %d", count)
	}
}

func TestRunCompaction_KeepsRunningTasks(t *testing.T) {
	s, cleanup := newTestDB(t)
	defer cleanup()
	db := s.DB

	old := time.Now().AddDate(0, 0, -40).Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`INSERT INTO tasks (task_id, status, updated_at) VALUES ('old_running', 'running', ?)`, old)
	_, _ = db.Exec(`INSERT INTO tasks (task_id, status, updated_at) VALUES ('old_completed', 'completed', ?)`, old)

	c := NewStorageCompactor(db, RetentionConfig{TaskHistoryDays: 30, MaxCompletedTasks: 1000})
	if err := c.RunCompaction(context.Background()); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_id = 'old_running'").Scan(&count)
	if count != 1 {
		t.Error("running task should NOT be pruned regardless of age")
	}
	db.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_id = 'old_completed'").Scan(&count)
	if count != 0 {
		t.Error("old completed task should be pruned")
	}
}

func TestAddPromotionAuditLog_FIFOCapping(t *testing.T) {
	sys := &config.SystemSettings{
		Retention: config.RetentionSettings{MaxPromotionAuditLogs: 3},
	}
	es, err := NewExperienceStore(t.TempDir(), nil, sys, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}

	for i := 0; i < 5; i++ {
		es.AddPromotionAuditLog(PromotionAuditLog{Action: "act_" + itoa(i)})
	}

	if len(es.PromotionAuditLogs) != 3 {
		t.Fatalf("expected 3 logs after capping, got %d", len(es.PromotionAuditLogs))
	}
	if es.PromotionAuditLogs[0].Action != "act_2" {
		t.Errorf("expected oldest surviving log act_2, got %s", es.PromotionAuditLogs[0].Action)
	}
	if es.PromotionAuditLogs[2].Action != "act_4" {
		t.Errorf("expected newest log act_4, got %s", es.PromotionAuditLogs[2].Action)
	}
}

func TestGetMaxPromotionAuditLogs_Default(t *testing.T) {
	es := newTestStore(t)
	if got := es.getMaxPromotionAuditLogs(); got != maxPromotionAuditLogsFallback {
		t.Errorf("default = %d, want %d", got, maxPromotionAuditLogsFallback)
	}
}

func TestGetMaxPromotionAuditLogs_FromSettings(t *testing.T) {
	sys := &config.SystemSettings{
		Retention: config.RetentionSettings{MaxPromotionAuditLogs: 42},
	}
	es, err := NewExperienceStore(t.TempDir(), nil, sys, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}
	if got := es.getMaxPromotionAuditLogs(); got != 42 {
		t.Errorf("from settings = %d, want 42", got)
	}
}
