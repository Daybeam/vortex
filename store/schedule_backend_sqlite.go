package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/daybeam/vortex/schemas"
)

type SQLiteScheduleBackend struct {
	db *sql.DB
}

func NewSQLiteScheduleBackend(db *sql.DB) *SQLiteScheduleBackend {
	return &SQLiteScheduleBackend{db: db}
}

func (b *SQLiteScheduleBackend) Save(ctx context.Context, s *schemas.Schedule) error {
	inputs, _ := json.Marshal(s.TaskInputs)
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO schedules (id, name, cron, task_request_json, last_run, next_run, is_enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			cron=excluded.cron,
			task_request_json=excluded.task_request_json,
			last_run=excluded.last_run,
			next_run=excluded.next_run,
			is_enabled=excluded.is_enabled
	`, s.ID, s.Name, s.CronExpr, string(inputs), s.LastRun, s.NextRun, s.Status == schemas.ScheduleActive, s.CreatedAt)
	return err
}

func (b *SQLiteScheduleBackend) LoadAll(ctx context.Context) ([]*schemas.Schedule, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, name, cron, task_request_json, last_run, next_run, is_enabled, created_at FROM schedules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*schemas.Schedule
	for rows.Next() {
		var s schemas.Schedule
		var inputs string
		var lastRun sql.NullTime
		var nextRun time.Time
		var isEnabled bool
		err := rows.Scan(&s.ID, &s.Name, &s.CronExpr, &inputs, &lastRun, &nextRun, &isEnabled, &s.CreatedAt)
		if err != nil {
			return nil, err
		}
		if lastRun.Valid {
			s.LastRun = &lastRun.Time
		}
		s.NextRun = nextRun
		if isEnabled {
			s.Status = schemas.ScheduleActive
		} else {
			s.Status = schemas.SchedulePaused
		}
		json.Unmarshal([]byte(inputs), &s.TaskInputs)
		results = append(results, &s)
	}
	return results, nil
}

func (b *SQLiteScheduleBackend) Delete(ctx context.Context, id string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	return err
}
