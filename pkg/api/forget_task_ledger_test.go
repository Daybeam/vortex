package api

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"

	_ "modernc.org/sqlite"
)

// Regression for O32: `/forget task <id>` deleted the archive entries and the
// artifacts directory — which holds manifest.json, the only resume source — but
// left the `tasks` row saying `running`. Nothing ever closed it: GetStatus kept
// answering "running" from the persisted row, and the compactor's terminal set
// never reclaimed it.

// TestForgetTask_MarksLedgerRowAborted drives the real slash command against a
// real SQLite `tasks` row. Before the fix the row stayed `running` after the
// confirm step.
func TestForgetTask_MarksLedgerRowAborted(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`CREATE TABLE tasks (
		task_id    TEXT PRIMARY KEY,
		status     TEXT NOT NULL DEFAULT 'running',
		graph_json BLOB,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	const payload = `{"task_id":"taskA"}`
	if _, err := db.Exec(`INSERT INTO tasks (task_id, status, graph_json) VALUES ('taskA', 'running', ?)`, payload); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	logDir, outDir := t.TempDir(), t.TempDir()
	logger, err := obs.NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	scheduler := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	defer scheduler.Stop()
	scheduler.SetTaskRegistry(store.NewSQLiteTaskBackend(db))

	h := newForgetTestHandler(t)
	h.App = &tools.App{Scheduler: scheduler}

	sessionID := "sess-forget-ledger"
	session := &chat.ChatSession{}

	if _, resp := h.handleSlashCommand("/forget task taskA", session, sessionID, "admin"); !strings.Contains(resp, "confirm") {
		t.Fatalf("step 1 should issue a confirmation challenge, got: %s", resp)
	}
	handled, resp := h.handleSlashCommand("/forget task taskA confirm", session, sessionID, "admin")
	if !handled {
		t.Fatal("expected /forget to be handled")
	}
	if !strings.Contains(resp, "✅") {
		t.Fatalf("expected success, got: %s", resp)
	}
	if !strings.Contains(resp, "aborted") {
		t.Errorf("expected the result line to report the ledger transition, got: %s", resp)
	}

	var status, graphJSON string
	if err := db.QueryRow(`SELECT status, graph_json FROM tasks WHERE task_id = 'taskA'`).Scan(&status, &graphJSON); err != nil {
		t.Fatalf("row should survive the forget (terminal status, not deletion): %v", err)
	}
	if status != "aborted" {
		t.Errorf("ledger status = %q, want aborted", status)
	}
	// The transition must be status-only: SaveTask would have overwritten the
	// graph payload with whatever the engine held in memory.
	if graphJSON != payload {
		t.Errorf("graph_json must be preserved, got %q", graphJSON)
	}
}

// TestForgetTask_WithoutSchedulerDoesNotClaimLedgerWrite: when no engine is
// attached the row cannot be reached, and the reply must not read as if it had
// been marked terminal.
func TestForgetTask_WithoutSchedulerDoesNotClaimLedgerWrite(t *testing.T) {
	h := newForgetTestHandler(t)
	// No App/Scheduler configured → the ledger cannot be reached at all.
	resp := h.forgetExecute("task", "taskA")
	if strings.Contains(resp, "aborted") {
		t.Errorf("must not claim a ledger transition it could not make: %s", resp)
	}
}
