package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// SQLiteTaskBackend implements ITaskBackend using SQLite.
type SQLiteTaskBackend struct {
	db *sql.DB
}

func NewSQLiteTaskBackend(db *sql.DB) *SQLiteTaskBackend {
	return &SQLiteTaskBackend{db: db}
}

func (b *SQLiteTaskBackend) Save(ctx context.Context, taskID, stepID string, data []byte) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO task_steps (task_id, step_id, status, result_json, updated_at)
		VALUES (?, ?, 'done', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(task_id, step_id) DO UPDATE SET
			status='done',
			result_json=excluded.result_json,
			updated_at=CURRENT_TIMESTAMP
	`, taskID, stepID, data)
	return err
}

func (b *SQLiteTaskBackend) Load(ctx context.Context, taskID, stepID string) ([]byte, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx, `
		SELECT result_json FROM task_steps
		WHERE task_id = ? AND step_id = ?
	`, taskID, stepID).Scan(&data)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("step not found: %s/%s", taskID, stepID)
	}
	return data, err
}

func (b *SQLiteTaskBackend) Delete(ctx context.Context, taskID string) (int, error) {
	res, err := b.db.ExecContext(ctx, `DELETE FROM task_steps WHERE task_id = ?`, taskID)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	return int(rows), nil
}

func (b *SQLiteTaskBackend) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	// Atomic Claim using status='pending' check
	res, err := b.db.ExecContext(ctx, `
		INSERT INTO task_steps (task_id, step_id, status, updated_at)
		VALUES (?, ?, 'running', CURRENT_TIMESTAMP)
		ON CONFLICT(task_id, step_id) DO UPDATE SET
			status='running',
			updated_at=CURRENT_TIMESTAMP
		WHERE task_steps.status = 'pending'
	`, taskID, stepID)

	if err != nil {
		return false, err
	}

	rows, _ := res.RowsAffected()
	if rows > 0 {
		return true, nil
	}

	// If 0 rows affected, it might already be running/done, or doesn't exist.
	// We need to check if it exists and is pending.
	// But according to the logic, if it didn't exist, INSERT would have succeeded (rows=1).
	// If it existed and was NOT pending, the WHERE clause would fail (rows=0).
	return false, nil
}

// ─── Task-level state ────────────────────────────────────────────────────
// task_steps only stores per-step results. Without task-level rows, GetStatus()
// cannot answer for graphs that are absent from the in-process map, which
// happens under the Master/Proxy split and after a restart.

// TaskRow is a lightweight projection of a persisted task graph.
type TaskRow struct {
	TaskID    string
	Status    string
	UpdatedAt string
}

// SaveTask upserts task-level state together with the serialized graph.
// It validates that immutable fields (e.g. require_human_approval) cannot be
// cleared once set, preventing tampering via graph overwrites.
func (b *SQLiteTaskBackend) SaveTask(ctx context.Context, taskID, status string, graphJSON []byte) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("SaveTask: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Immutability check: load existing graph and compare require_human_approval
	var existingJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT graph_json FROM tasks WHERE task_id = ?`, taskID).Scan(&existingJSON)
	if err == nil && existingJSON != nil {
		var oldGraph, newGraph schemas.TaskGraph
		if json.Unmarshal(existingJSON, &oldGraph) == nil && json.Unmarshal(graphJSON, &newGraph) == nil {
			for stepID, oldStep := range oldGraph.Steps {
				if oldStep.RequireHumanApproval {
					if newStep, ok := newGraph.Steps[stepID]; ok && !newStep.RequireHumanApproval {
						return fmt.Errorf("immutable field violation: require_human_approval cannot be cleared")
					}
				}
			}
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks (task_id, status, graph_json, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(task_id) DO UPDATE SET
			status=excluded.status,
			graph_json=excluded.graph_json,
			updated_at=CURRENT_TIMESTAMP
	`, taskID, status, graphJSON)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateTaskStatus refreshes only the lifecycle status, leaving any previously
// persisted graph payload intact.
func (b *SQLiteTaskBackend) UpdateTaskStatus(ctx context.Context, taskID, status string) error {
	_, err := b.db.ExecContext(ctx,
		`UPDATE tasks SET status=?, updated_at=CURRENT_TIMESTAMP WHERE task_id=?`,
		status, taskID)
	return err
}

// LoadTask returns the task row plus its serialized graph (nil when absent).
func (b *SQLiteTaskBackend) LoadTask(ctx context.Context, taskID string) (*TaskRow, []byte, error) {
	var row TaskRow
	var graphJSON []byte
	err := b.db.QueryRowContext(ctx, `
		SELECT task_id, status, graph_json, updated_at FROM tasks WHERE task_id = ?
	`, taskID).Scan(&row.TaskID, &row.Status, &graphJSON, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("task not found: %s", taskID)
	}
	if err != nil {
		return nil, nil, err
	}
	return &row, graphJSON, nil
}

// ListTasks returns the most recently updated tasks, newest first.
func (b *SQLiteTaskBackend) ListTasks(ctx context.Context, limit int) ([]TaskRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := b.db.QueryContext(ctx, `
		SELECT task_id, status, updated_at FROM tasks ORDER BY updated_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TaskRow{}
	for rows.Next() {
		var r TaskRow
		if err := rows.Scan(&r.TaskID, &r.Status, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
