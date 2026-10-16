package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// CostTracker records per-LLM-call token spend to SQLite for operations
// visibility. Each call logs model, taskID, stepID, input/output tokens,
// and timestamp. Independent of Step Plan — this is a global ops function.
//
// Design: docs/plan/STEP_PLAN_MODE_DESIGN.md §13.7 (item #18, Intrect-io CostTracker).
type CostTracker struct {
	mu sync.Mutex
	db *sql.DB
}

// CostRecord is one logged LLM call's token spend.
type CostRecord struct {
	Timestamp    time.Time
	Model        string
	TaskID       string
	StepID       string
	InputTokens  int
	OutputTokens int
}

// NewCostTracker opens (or creates) a SQLite database at dbPath and
// ensures the cost_records table exists. Nil-safe: returns nil if dbPath
// is empty (cost tracking disabled).
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
		output_tokens INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("cost_tracker: create table: %w", err)
	}
	return &CostTracker{db: db}, nil
}

// Record logs one LLM call's token spend. Best-effort: errors are returned
// but callers typically ignore them (a failed cost log must not break the
// LLM call flow).
func (ct *CostTracker) Record(model, taskID, stepID string, inputTokens, outputTokens int) error {
	if ct == nil || ct.db == nil {
		return nil
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // audit P-LOW-3: prevent indefinite hang on slow DB
	defer cancel()
	_, err := ct.db.ExecContext(ctx,
		`INSERT INTO cost_records (ts, model, task_id, step_id, input_tokens, output_tokens) VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), model, taskID, stepID, inputTokens, outputTokens,
	)
	return err
}

// GetTotalSpend returns a map of model → total tokens (input + output).
func (ct *CostTracker) GetTotalSpend() (map[string]int, error) {
	if ct == nil || ct.db == nil {
		return nil, nil
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // audit P-LOW-3: prevent indefinite hang on slow DB
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
