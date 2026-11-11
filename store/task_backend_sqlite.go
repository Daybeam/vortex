package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// LoadBatch fetches multiple step results in a single SQL query (audit P-H1-H4).
// Uses `SELECT ... WHERE step_id IN (?,?,...)` instead of N separate queries.
// SQLite default host-parameter limit is 999; callers with >900 stepIDs should chunk.
func (b *SQLiteTaskBackend) LoadBatch(ctx context.Context, taskID string, stepIDs []string) (map[string][]byte, error) {
	if len(stepIDs) == 0 {
		return make(map[string][]byte), nil
	}

	// Build dynamic IN clause: (?, ?, ?, ...)
	placeholders := make([]string, len(stepIDs))
	args := make([]any, 0, len(stepIDs)+1)
	args = append(args, taskID)
	for i, sid := range stepIDs {
		placeholders[i] = "?"
		args = append(args, sid)
	}

	query := fmt.Sprintf(`
		SELECT step_id, result_json FROM task_steps
		WHERE task_id = ? AND step_id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]byte, len(stepIDs))
	for rows.Next() {
		var stepID string
		var data []byte
		if err := rows.Scan(&stepID, &data); err != nil {
			return nil, err
		}
		result[stepID] = data
	}
	return result, rows.Err()
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

var _ StaleTaskSweeper = (*SQLiteTaskBackend)(nil)

// staleRunningPredicate is the one definition of "this row may be closed":
// still running, and nobody has written it since the cutoff. The SELECT and the
// guarded UPDATE both splice it in, so a close can never rest on a row the
// SELECT was not allowed to name. Same shape as claimablePredicate in
// chat_outbox_sqlite.go, and it needs no holder column: updated_at is already
// the row's liveness signal, because every SaveTask/UpdateTaskStatus bumps it.
const staleRunningPredicate = `status = 'running' AND updated_at < ?`

// SweepStaleRunning closes ledger rows orphaned by a dead process. The engine's
// in-process monitor walks its own graph map, so after a restart these rows are
// invisible to it and the frontend keeps polling a task that will never finish.
func (b *SQLiteTaskBackend) SweepStaleRunning(ctx context.Context, cutoff time.Time) ([]string, error) {
	// updated_at is written by CURRENT_TIMESTAMP, i.e. UTC text in this
	// fixed-width layout, so the comparison is lexicographic. Binding a local
	// time would shift the cutoff by the host offset and close rows that are
	// not stale yet — on a +08:00 host, eight hours early.
	cutoffText := cutoff.UTC().Format("2006-01-02 15:04:05")

	rows, err := b.db.QueryContext(ctx, `
		SELECT task_id FROM tasks WHERE `+staleRunningPredicate+` ORDER BY updated_at
	`, cutoffText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var closed []string
	for _, taskID := range ids {
		res, err := b.db.ExecContext(ctx, `
			UPDATE tasks SET status = 'failed', updated_at = CURRENT_TIMESTAMP
			WHERE task_id = ? AND `+staleRunningPredicate, taskID, cutoffText)
		if err != nil {
			return closed, err
		}
		// Zero rows means a live hub persisted progress, or another hub's sweep
		// closed it, between the SELECT and this UPDATE. Skipping is the whole
		// multi-hub guarantee: no mutex is involved, so it holds across
		// processes sharing this file.
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		closed = append(closed, taskID)
	}
	return closed, nil
}
