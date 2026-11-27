package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// CostTracker records per-LLM-call token spend to SQLite for operations
// visibility. Each call logs model, taskID, stepID, input/output tokens,
// and timestamp. Independent of Step Plan — this is a global ops function.
//
// Design: docs/completed/2026-10-26/STEP_PLAN_MODE_DESIGN.md §13.7 (item #18, Intrect-io CostTracker).
type CostTracker struct {
	db *sql.DB
}

// CostRecord is one logged LLM call's token spend.
type CostRecord struct {
	Timestamp        time.Time
	Model            string
	TaskID           string
	StepID           string
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
}

// NewCostTracker creates a new CostTracker with SQLite backend.
// An empty dbPath disables cost tracking (returns a nil tracker, nil error).
// Otherwise the caller MUST call Close() when done to release the database
// connection.
func NewCostTracker(dbPath string) (*CostTracker, error) {
	if dbPath == "" {
		return nil, nil
	}
	dir := filepath.Dir(dbPath)
	if dir != "." && dir != "" {
		os.MkdirAll(dir, 0755)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cost_tracker: open db: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("cost_tracker: ping: %w", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS cost_records (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		ts         TEXT    NOT NULL,
		model      TEXT    NOT NULL,
		task_id    TEXT    NOT NULL,
		step_id    TEXT    NOT NULL,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens  INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("cost_tracker: create table: %w", err)
	}
	if err := ensureCostLedgerColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("cost_tracker: migrate table: %w", err)
	}
	return &CostTracker{db: db}, nil
}

func ensureCostLedgerColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(cost_records)`)
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, column := range []string{"cache_write_tokens", "cache_read_tokens"} {
		if !columns[column] {
			if _, err := db.Exec(`ALTER TABLE cost_records ADD COLUMN ` + column + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	}
	return nil
}

// Record logs one LLM call's token spend. Best-effort: errors are returned
// but callers typically ignore them (a failed cost log must not break the
// LLM call flow).
func (ct *CostTracker) Record(model, taskID, stepID string, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens int) error {
	if ct == nil || ct.db == nil {
		return nil
	}
	// No mutex: SQLite WAL mode + busy_timeout(5000) handles concurrent writes.
	// The mutex previously serialized all Record/GetTotalSpend calls, making
	// cost tracking a bottleneck under high task throughput.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ct.db.ExecContext(ctx,
		`INSERT INTO cost_records (ts, model, task_id, step_id, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), model, taskID, stepID, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens,
	)
	return err
}

// GetTotalSpend returns a map of model → total tokens (input + output).
func (ct *CostTracker) GetTotalSpend() (map[string]int, error) {
	if ct == nil || ct.db == nil {
		return nil, nil
	}
	// No mutex: SQLite WAL mode handles concurrent read/write.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := ct.db.QueryContext(ctx, `SELECT model, SUM(input_tokens + output_tokens) FROM cost_records GROUP BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var model string
		var total int
		if err := rows.Scan(&model, &total); err != nil {
			return nil, err
		}
		result[model] = total
	}
	return result, nil
}

// Close releases the database connection.
func (ct *CostTracker) Close() {
	if ct != nil && ct.db != nil {
		ct.db.Close()
	}
}

// SetCostTracker wires a CostTracker into the scheduler for per-call token spend logging.
// Call once after NewDirectedEngine; nil-safe (passing nil disables tracking).
func (s *DirectedEngine) SetCostTracker(ct *CostTracker) {
	s.costTracker = ct
}
