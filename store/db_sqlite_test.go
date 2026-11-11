package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestInitDB_ConcurrentWritesDoNotHitSQLiteBusy reproduces this project's own
// verified concurrent-step-execution pattern (DirectedEngine goroutines,
// parallel role_group dispatch -- see the 2026-08-01 addendum) directly
// against the runtime DB. Without SetMaxOpenConns(1)+busy_timeout, this test
// is expected to intermittently fail with "database is locked" errors from
// pooled connections racing each other; with the fix, every write should
// either succeed or wait, never fail with SQLITE_BUSY.
func TestInitDB_ConcurrentWritesDoNotHitSQLiteBusy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrency_test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS concurrency_probe (id INTEGER PRIMARY KEY AUTOINCREMENT, val TEXT)"); err != nil {
		t.Fatalf("failed to create probe table: %v", err)
	}

	const goroutines = 20
	const writesEach = 5
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*writesEach)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for w := 0; w < writesEach; w++ {
				if _, err := db.Exec("INSERT INTO concurrency_probe (val) VALUES (?)", "g"); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent write failed (expected none with SetMaxOpenConns(1)+busy_timeout): %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM concurrency_probe").Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != goroutines*writesEach {
		t.Errorf("expected %d rows, got %d (some writes silently lost)", goroutines*writesEach, count)
	}
}

// TestInitDB_SetsConnectionPool guards the specific configuration this
// project relies on -- WAL mode supports concurrent readers, so we allow
// a small pool of connections instead of serializing through one.
func TestInitDB_SetsConnectionPool(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pool_test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer db.Close()

	stats := db.Stats()
	if stats.MaxOpenConnections != 10 {
		t.Errorf("expected MaxOpenConnections == 10, got %d", stats.MaxOpenConnections)
	}
}

// TestInitDB_FailureReturnsNilDB pins the error-path contract for audit
// MEDIUM-007: when the DSN is unopenable, InitDB must return (nil, error) so
// callers skip db.Close() on a pool InitDB has already released.
// See QUALITY_AUDIT_REPORT.md MEDIUM-007.
func TestInitDB_FailureReturnsNilDB(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "not_a_db.db")
	if err := os.WriteFile(badPath, []byte("this is not a sqlite database"), 0o644); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}

	db, err := InitDB(badPath)
	if err == nil {
		db.Close()
		t.Fatal("expected InitDB to fail on a corrupt database file")
	}
	if db != nil {
		t.Fatalf("expected nil *sql.DB on error, got %v", db)
	}
}

// TestInitDB_BackfillsColumnsOnLegacyDB reproduces F13 in
// docs/plan/TELEGRAM_CALLBACK_AND_VCP_FINDINGS_2026-11-10.md, measured on the
// live hub 2026-11-10: "failed to run migrations: SQL logic error: no such
// column: domain". A column added to CREATE TABLE IF NOT EXISTS never lands on
// a DB that already has the table, so idx_exp_nodes_domain failed, and because
// the whole schema is one multi-statement Exec that error aborted everything
// after it. InitDB then returned nil and every entry point fell back to file
// mode with a nil task registry — which is what made the F10 stale-row sweep
// unreachable in production even though its own tests passed.
func TestInitDB_BackfillsColumnsOnLegacyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// The column sets as they stood before the 2026-11-09 domain facet and
	// before 70f189c82 (metabolic skill columns). Rows are seeded first so the
	// backfill defaults are checked against data that predates the columns,
	// not only against inserts made afterwards.
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE experience_nodes (
			node_id TEXT PRIMARY KEY, task_id TEXT NOT NULL, step_id TEXT, role_id TEXT,
			capability TEXT, strategy TEXT, action TEXT, outcome TEXT, error_signal TEXT,
			model_id TEXT, failure_mode TEXT, critique TEXT, confidence REAL DEFAULT 0,
			source_text TEXT, embedding BLOB, embedding_model TEXT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE generated_skills (
			id TEXT PRIMARY KEY, capability TEXT NOT NULL, description TEXT,
			usage_count INTEGER DEFAULT 0, success_rate REAL DEFAULT 0,
			is_verified BOOLEAN DEFAULT FALSE, criticality REAL DEFAULT 0,
			parent_id TEXT, os TEXT, arch TEXT, shell TEXT, failure_signal TEXT,
			source_text TEXT, embedding BLOB, embedding_model TEXT, last_used DATETIME)`,
		`INSERT INTO experience_nodes (node_id, task_id, outcome) VALUES ('n_legacy', 't_legacy', 'ok')`,
		`INSERT INTO generated_skills (id, capability, usage_count) VALUES ('s_legacy', 'coding', 3)`,
	} {
		if _, err := legacy.Exec(ddl); err != nil {
			legacy.Close()
			t.Fatalf("seed legacy schema: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB must repair a legacy DB, got: %v", err)
	}
	defer db.Close()

	var idx string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_exp_nodes_domain'`).Scan(&idx); err != nil {
		t.Errorf("the index that aborted the migration is still missing: %v", err)
	}

	var cost int64
	var latency, roi float64
	var status string
	if err := db.QueryRow(`SELECT token_cost_total, avg_latency_ms, metabolic_roi, skill_status
		FROM generated_skills WHERE id='s_legacy'`).Scan(&cost, &latency, &roi, &status); err != nil {
		t.Fatalf("metabolic columns unusable on a pre-existing row: %v", err)
	}
	if cost != 0 || latency != 0 || roi != 1.0 || status != "active" {
		t.Errorf("backfill defaults = (%d,%v,%v,%q), want (0,0,1,active)", cost, latency, roi, status)
	}

	var domain sql.NullString
	if err := db.QueryRow(`SELECT domain FROM experience_nodes WHERE node_id='n_legacy'`).Scan(&domain); err != nil {
		t.Fatalf("domain column unusable on a pre-existing row: %v", err)
	}
	if domain.Valid {
		t.Errorf("legacy row domain = %q, want NULL", domain.String)
	}

	if _, err := db.Exec(`INSERT INTO experience_nodes (node_id, task_id, domain) VALUES ('n_new','t_new','finance')`); err != nil {
		t.Errorf("insert using backfilled domain failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO generated_skills (id, capability, skill_status) VALUES ('s_new','coding','retired')`); err != nil {
		t.Errorf("insert using backfilled skill_status failed: %v", err)
	}

	// Re-running on the repaired file must not fail with "duplicate column name".
	again, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("second InitDB must be idempotent, got: %v", err)
	}
	again.Close()
}
