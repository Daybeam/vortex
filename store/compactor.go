package store

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/daybeam/vortex/config"
)

// RetentionConfig is the store-level compaction configuration, decoupled from
// the config package's RetentionSettings to avoid a circular import.
// See docs/architecture/STORAGE_COMPACTION_DESIGN.md.
type RetentionConfig struct {
	TaskHistoryDays        int
	StagingTTLHours        int
	MaxCompletedTasks      int
	MaxPromotionAuditLogs  int
	AutoVacuumIntervalDays int
	VacuumOnStartup        bool
}

type StorageCompactor struct {
	db     *sql.DB
	config RetentionConfig
}

func NewStorageCompactor(db *sql.DB, cfg RetentionConfig) *StorageCompactor {
	return &StorageCompactor{db: db, config: cfg}
}

// NewStorageCompactorFromSettings builds a StorageCompactor from config
// SystemSettings, applying headless zero-config defaults for any unset field.
func NewStorageCompactorFromSettings(db *sql.DB, sys *config.SystemSettings) *StorageCompactor {
	cfg := RetentionConfig{}
	if sys != nil {
		cfg.TaskHistoryDays = sys.Retention.TaskHistoryDays
		cfg.StagingTTLHours = sys.Retention.StagingTTLHours
		cfg.MaxCompletedTasks = sys.Retention.MaxCompletedTasks
		cfg.MaxPromotionAuditLogs = sys.Retention.MaxPromotionAuditLogs
		cfg.AutoVacuumIntervalDays = sys.Retention.AutoVacuumIntervalDays
		cfg.VacuumOnStartup = sys.Retention.VacuumOnStartup
	}
	if cfg.TaskHistoryDays == 0 {
		cfg.TaskHistoryDays = 30
	}
	if cfg.StagingTTLHours == 0 {
		cfg.StagingTTLHours = 168
	}
	if cfg.MaxCompletedTasks == 0 {
		cfg.MaxCompletedTasks = 1000
	}
	if cfg.MaxPromotionAuditLogs == 0 {
		cfg.MaxPromotionAuditLogs = 1000
	}
	if cfg.AutoVacuumIntervalDays == 0 {
		cfg.AutoVacuumIntervalDays = 7
	}
	return NewStorageCompactor(db, cfg)
}

// RunCompaction performs SQLite pruning, table vacuuming, and stale data removal.
func (c *StorageCompactor) RunCompaction(ctx context.Context) error {
	log.Printf("[Compactor] Starting storage compaction pass...")

	if c.config.TaskHistoryDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -c.config.TaskHistoryDays).Format("2006-01-02 15:04:05")
		_, err := c.db.ExecContext(ctx, `DELETE FROM tasks WHERE updated_at < ? AND status IN ('completed', 'failed', 'aborted')`, cutoff)
		if err != nil {
			log.Printf("[Compactor] WARN: failed to prune old tasks: %v", err)
		}
		_, _ = c.db.ExecContext(ctx, `DELETE FROM task_steps WHERE task_id NOT IN (SELECT task_id FROM tasks)`)
	}

	if c.config.MaxCompletedTasks > 0 {
		_, err := c.db.ExecContext(ctx, `
			DELETE FROM tasks
			WHERE status IN ('completed', 'failed', 'aborted')
			  AND task_id NOT IN (
			      SELECT task_id FROM tasks
				  WHERE status IN ('completed', 'failed', 'aborted')
				  ORDER BY updated_at DESC
				  LIMIT ?
			  )
		`, c.config.MaxCompletedTasks)
		if err != nil {
			log.Printf("[Compactor] WARN: failed to prune excess tasks (FIFO): %v", err)
		}
		_, _ = c.db.ExecContext(ctx, `DELETE FROM task_steps WHERE task_id NOT IN (SELECT task_id FROM tasks)`)
	}

	if _, err := c.db.ExecContext(ctx, `PRAGMA incremental_vacuum;`); err != nil {
		if _, errFull := c.db.ExecContext(ctx, `VACUUM;`); errFull != nil {
			log.Printf("[Compactor] WARN: SQLite vacuum failed: %v", errFull)
		} else {
			log.Printf("[Compactor] SQLite full VACUUM completed.")
		}
	} else {
		log.Printf("[Compactor] SQLite incremental_vacuum completed.")
	}

	log.Printf("[Compactor] Storage compaction pass finished successfully.")
	return nil
}

// Start runs the compaction loop in a background goroutine on a fixed interval
// derived from AutoVacuumIntervalDays. Returns immediately.
func (c *StorageCompactor) Start(ctx context.Context) {
	interval := time.Duration(c.config.AutoVacuumIntervalDays) * 24 * time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.RunCompaction(ctx); err != nil {
				log.Printf("[Compactor] background pass failed: %v", err)
			}
		}
	}
}
