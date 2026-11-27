package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
)

// Regression for docs/plan/ENGINE_EVAL_VALIDATION_PLAN-2026-11-27.md §F8:
// provider cache usage must survive output parsing and SQLite ledger storage.
func TestF8_ProviderCacheUsageReachesLedger(t *testing.T) {
	spawner := &Spawner{registry: &config.Registry{System: config.SystemSettings{}}}
	role := &config.Role{BaseCapability: "general"}
	req := &SpawnRequest{TaskID: "task-cache", StepID: "step-cache"}
	resp := &providers.ProviderResponse{
		Text:             `{"status":"ok","confidence":0.9,"result":{"ok":true}}`,
		PromptTokens:     1000,
		CompletionTokens: 100,
		CacheWriteTokens: 200,
		CacheReadTokens:  600,
	}
	outcome := spawner.completeTurn(context.Background(), req, role, resp, &spawnTurnState{}, 0, 1, "", "provider", "model", nil, nil, 3)
	if !outcome.done || outcome.result == nil || outcome.result.Output.Usage == nil {
		t.Fatalf("completeTurn did not return usage: %+v", outcome)
	}
	usage := outcome.result.Output.Usage
	if usage.CacheWriteTokens != 200 || usage.CacheReadTokens != 600 {
		t.Fatalf("cache usage = write %d/read %d, want 200/600", usage.CacheWriteTokens, usage.CacheReadTokens)
	}

	ct, err := NewCostTracker(filepath.Join(t.TempDir(), "cost.db"))
	if err != nil {
		t.Fatalf("NewCostTracker: %v", err)
	}
	defer ct.Close()
	if err := ct.Record("model", req.TaskID, req.StepID, usage.PromptTokens, usage.CompletionTokens, usage.CacheWriteTokens, usage.CacheReadTokens); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var write, read int
	if err := ct.db.QueryRow(`SELECT cache_write_tokens, cache_read_tokens FROM cost_records WHERE task_id = ? AND step_id = ?`, req.TaskID, req.StepID).Scan(&write, &read); err != nil {
		t.Fatalf("query cache ledger: %v", err)
	}
	if write != 200 || read != 600 {
		t.Fatalf("ledger cache usage = write %d/read %d, want 200/600", write, read)
	}
}

// Regression for docs/plan/ENGINE_EVAL_VALIDATION_PLAN-2026-11-27.md §F8:
// existing installations retain their old ledger and gain cache columns on open.
func TestF8_LegacyCostLedgerMigratesWithoutChangingSpend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy ledger: %v", err)
	}
	_, err = legacy.Exec(`CREATE TABLE cost_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL,
		model TEXT NOT NULL,
		task_id TEXT NOT NULL,
		step_id TEXT NOT NULL,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO cost_records (ts, model, task_id, step_id, input_tokens, output_tokens)
	VALUES ('2026-11-27T00:00:00Z', 'model', 'old-task', 'old-step', 1000, 100);`)
	if err != nil {
		legacy.Close()
		t.Fatalf("create legacy ledger: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy ledger: %v", err)
	}

	ct, err := NewCostTracker(path)
	if err != nil {
		t.Fatalf("NewCostTracker migration: %v", err)
	}
	defer ct.Close()
	if err := ct.Record("model", "new-task", "new-step", 500, 50, 100, 300); err != nil {
		t.Fatalf("Record after migration: %v", err)
	}
	var oldWrite, oldRead, newWrite, newRead int
	if err := ct.db.QueryRow(`SELECT cache_write_tokens, cache_read_tokens FROM cost_records WHERE task_id = 'old-task'`).Scan(&oldWrite, &oldRead); err != nil {
		t.Fatalf("query migrated old row: %v", err)
	}
	if oldWrite != 0 || oldRead != 0 {
		t.Fatalf("old row cache usage = write %d/read %d, want 0/0", oldWrite, oldRead)
	}
	if err := ct.db.QueryRow(`SELECT cache_write_tokens, cache_read_tokens FROM cost_records WHERE task_id = 'new-task'`).Scan(&newWrite, &newRead); err != nil {
		t.Fatalf("query new row: %v", err)
	}
	if newWrite != 100 || newRead != 300 {
		t.Fatalf("new row cache usage = write %d/read %d, want 100/300", newWrite, newRead)
	}
	spend, err := ct.GetTotalSpend()
	if err != nil {
		t.Fatalf("GetTotalSpend: %v", err)
	}
	if spend["model"] != 1650 {
		t.Fatalf("spend = %d, want 1650 input+output tokens", spend["model"])
	}
}
