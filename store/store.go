package store

import (
	"database/sql"
	"github.com/daybeam/vortex/config"
	"sync"
)

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
