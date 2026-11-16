package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newSweepTestDB(t *testing.T) *SQLiteTaskBackend {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSQLiteTaskBackend(db)
}

// agedTimestamp renders the fixed-width UTC text CURRENT_TIMESTAMP produces, so
// a row can be aged without waiting for the clock.
func agedTimestamp(d time.Duration) string {
	return time.Now().UTC().Add(d).Format("2006-01-02 15:04:05")
}

// TestSweepStaleRunning_ClosesOnlyOrphanedRunningRows is the F10 regression at
// the store layer: a row left 'running' by a dead process must close, while a
// live row and every terminal row must be left exactly as they were.
// Bug: docs/completed/2026-11-15/TELEGRAM_CALLBACK_AND_VCP_FINDINGS_2026-11-10.md F10.
func TestSweepStaleRunning_ClosesOnlyOrphanedRunningRows(t *testing.T) {
	b := newSweepTestDB(t)
	ctx := context.Background()
	stale, fresh := agedTimestamp(-96*time.Hour), agedTimestamp(0)

	for _, row := range []struct{ id, status, at string }{
		{"orphan", "running", stale},
		{"live", "running", fresh},
		{"done", "completed", stale},
		{"dead", "failed", stale},
		{"gone", "aborted", stale},
	} {
		if _, err := b.db.ExecContext(ctx,
			`INSERT INTO tasks (task_id, status, updated_at) VALUES (?, ?, ?)`,
			row.id, row.status, row.at); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	closed, err := b.SweepStaleRunning(ctx, cutoff)
	if err != nil {
		t.Fatalf("SweepStaleRunning: %v", err)
	}
	if len(closed) != 1 || closed[0] != "orphan" {
		t.Fatalf("closed = %v, want [orphan]", closed)
	}

	for _, want := range []struct{ id, status string }{
		{"orphan", "failed"}, {"live", "running"},
		{"done", "completed"}, {"dead", "failed"}, {"gone", "aborted"},
	} {
		var got string
		if err := b.db.QueryRowContext(ctx,
			`SELECT status FROM tasks WHERE task_id = ?`, want.id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", want.id, err)
		}
		if got != want.status {
			t.Errorf("task %s status = %s, want %s", want.id, got, want.status)
		}
	}

	// Idempotent: a second pass finds nothing, so an hourly tick cannot re-close
	// or re-report the same row.
	again, err := b.SweepStaleRunning(ctx, cutoff)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second sweep closed %v, want none", again)
	}
}

// TestSweepStaleRunning_LiveHubWriteProtectsRow is the multi-hub safety case:
// a hub that is still driving the task keeps bumping updated_at, so the row must
// not qualify even though it was stale when an operator looked at it earlier.
func TestSweepStaleRunning_LiveHubWriteProtectsRow(t *testing.T) {
	b := newSweepTestDB(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO tasks (task_id, status, updated_at) VALUES ('adopted', 'running', ?)`,
		agedTimestamp(-96*time.Hour)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The live hub persists progress and leaves the task running.
	if err := b.SaveTask(ctx, "adopted", "running", []byte(`{"task_id":"adopted"}`)); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}

	closed, err := b.SweepStaleRunning(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("SweepStaleRunning: %v", err)
	}
	if len(closed) != 0 {
		t.Fatalf("closed %v, want none: a live hub just wrote this row", closed)
	}
	row, _, err := b.LoadTask(ctx, "adopted")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if row.Status != "running" {
		t.Errorf("status = %s, want running", row.Status)
	}
}

// TestSweepStaleRunning_AtMostOnceAcrossHubs proves the fence is in the SQL and
// not in a process-local mutex: two hubs sharing one DB file sweep the same rows
// at the same time, and each row is reported closed exactly once.
func TestSweepStaleRunning_AtMostOnceAcrossHubs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "shared.db")
	seed, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	ctx := context.Background()
	const rows = 5
	stale := agedTimestamp(-96 * time.Hour)
	for i := 0; i < rows; i++ {
		if _, err := seed.ExecContext(ctx,
			`INSERT INTO tasks (task_id, status, updated_at) VALUES (?, 'running', ?)`,
			fmt.Sprintf("orphan_%d", i), stale); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	seed.Close()

	// Two independent handles on one file, the shape of two hubs on one host.
	open := func() *SQLiteTaskBackend {
		db, err := InitDB(dbPath)
		if err != nil {
			t.Fatalf("InitDB: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return NewSQLiteTaskBackend(db)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, b := range []*SQLiteTaskBackend{open(), open()} {
		wg.Add(1)
		go func(b *SQLiteTaskBackend) {
			defer wg.Done()
			closed, err := b.SweepStaleRunning(ctx, cutoff)
			if err != nil {
				t.Errorf("sweep: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range closed {
				seen[id]++
			}
		}(b)
	}
	wg.Wait()

	if len(seen) != rows {
		t.Fatalf("reported %d distinct rows, want %d (seen=%v)", len(seen), rows, seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s reported closed %d times, want exactly 1", id, n)
		}
	}
}
