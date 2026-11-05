package store

import (
	"database/sql"
	"fmt"

	"github.com/daybeam/vortex/config"
	"sync"
)

// FileFallbackWarning spells out what a failed InitDB actually costs. The four
// entry points used to log only "Falling back to File storage", which read as if
// nothing were lost. Most things are not lost -- tasks, schedules, memory-bank
// markdown and the whole experience store (its JSON snapshots cover every
// collection load() reads) keep working on disk. The SQLite-only areas below go
// dark, so an operator needs to know they are off. Lives in store rather than in
// main_*.go so the list has one source; file_mode_test.go pins it against what
// NewStore really leaves nil.
func FileFallbackWarning(dbErr error) string {
	return fmt.Sprintf("failed to init sqlite db: %v. File mode keeps tasks, schedules, "+
		"memory-bank markdown and the experience store, but these SQLite-only areas are OFF: "+
		"task lifecycle registry (task status is memory-only), baseline store, "+
		"role/SOP database sync, capability profiles (theta/Pareto routing has nothing to read), "+
		"anti-pattern precedents (never saved, never loaded), decision outcomes, "+
		"memory-bank item mirror with semantic search and profile deletion, "+
		"stored OAuth tokens (lost on restart), and storage compaction", dbErr)
}

// Store is the high-level coordinator for all storage modules.
type Store struct {
	Tasks        ITaskStore
	Experience   IExperienceStore
	Schedules    *ScheduleStore
	Config       ConfigRepository
	AntiPatterns *AntiPatternStore
	MemoryBank   *MemoryBankStore
	DB           *sql.DB
	// TaskRegistry persists task-level lifecycle state (separate from the
	// step-scoped Tasks store). Nil in File mode.
	TaskRegistry TaskRegistry
	mu           sync.RWMutex
}

func NewStore(root, expDir, schedulePath string, sys *config.SystemSettings, db *sql.DB) (*Store, error) {
	var backend ITaskBackend
	var expBackend IExperienceBackend
	var scheduleBackend IScheduleBackend
	var mbBackend IMemoryBankBackend
	var apBackend IAntiPatternBackend
	var configRepo ConfigRepository
	if db != nil {
		backend = NewSQLiteTaskBackend(db)
		expBackend = NewSQLiteExperienceBackend(db)
		scheduleBackend = NewSQLiteScheduleBackend(db)
		mbBackend = NewSQLiteMemoryBankBackend(db)
		apBackend = NewSQLiteAntiPatternBackend(db)
		configRepo = NewSQLiteRepo(db)
	} else {
		backend = NewFileTaskBackend("tasks")
		expBackend = &FileExperienceBackend{dir: expDir}
		// others remain nil for File mode
	}
	ts := NewTaskStore(backend)

	es, err := NewExperienceStore(expDir, ts, sys, expBackend, apBackend)
	if err != nil {
		return nil, err
	}

	ss := NewScheduleStore(schedulePath, scheduleBackend)
	mb := NewMemoryBankStore(root, mbBackend)

	// Task-level lifecycle state is only available with SQLite backing. In File
	// mode this stays nil and consumers fall back to memory-only status.
	var taskRegistry TaskRegistry
	if db != nil {
		taskRegistry = NewSQLiteTaskBackend(db)
	}

	return &Store{
		Tasks:        ts,
		Experience:   es,
		Schedules:    ss,
		Config:       configRepo,
		AntiPatterns: es.AntiPatternStore,
		MemoryBank:   mb,
		DB:           db,
		TaskRegistry: taskRegistry,
	}, nil
}
