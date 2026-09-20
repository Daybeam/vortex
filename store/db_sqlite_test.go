package store

import (
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
