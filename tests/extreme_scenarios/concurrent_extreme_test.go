package extreme_tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openSQLiteWithBusyTimeout opens a SQLite DB with a 5s busy timeout
// to handle concurrent write contention (SQLITE_BUSY retry).
func openSQLiteWithBusyTimeout(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	return db, nil
}

// TestExtreme_100ConcurrentFileWrites verifies that 100 goroutines
// writing to different files complete without deadlock or race.
// Uses -race flag to detect data races.
//
// Extreme scenario: 100 concurrent tasks each writing output files.
func TestExtreme_100ConcurrentFileWrites(t *testing.T) {
	tmpDir := t.TempDir()
	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			path := filepath.Join(tmpDir, fmt.Sprintf("output_%d.txt", id))
			data := fmt.Sprintf("task %d result data", id)
			if err := os.WriteFile(path, []byte(data), 0644); err != nil {
				errCh <- fmt.Errorf("task %d: %w", id, err)
				return
			}
			errCh <- nil
		}(i)
	}

	wg.Wait()
	close(errCh)

	failures := 0
	for err := range errCh {
		if err != nil {
			failures++
		}
	}
	if failures > 0 {
		t.Errorf("%d/100 concurrent writes failed", failures)
	}
}

// TestExtreme_100ConcurrentContextCancellation verifies that 100
// goroutines with context cancellation all exit promptly when cancelled.
//
// Extreme scenario: Shutdown signal arrives while 100 tasks are running.
func TestExtreme_100ConcurrentContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	done := make(chan int, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				done <- id
				return
			case <-time.After(10 * time.Second):
				// Should not reach here — cancel should fire first
			}
		}(i)
	}

	// Cancel after 100ms — all 100 goroutines should exit
	time.Sleep(100 * time.Millisecond)
	cancel()

	completed := 0
	doneChan := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneChan)
	}()

	select {
	case <-doneChan:
		// All goroutines exited
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for 100 goroutines to exit after cancel")
	}

	for i := 0; i < 100; i++ {
		select {
		case <-done:
			completed++
		default:
		}
	}
	if completed != 100 {
		t.Errorf("only %d/100 goroutines exited after cancel", completed)
	}
}

// TestExtreme_SQLiteWriteContention verifies that concurrent writes
// to the same SQLite database don't deadlock (uses busy_timeout).
//
// Extreme scenario: Multiple goroutines writing to the same SQLite DB.
func TestExtreme_SQLiteWriteContention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := openSQLiteWithBusyTimeout(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS tasks (id INTEGER PRIMARY KEY, data TEXT)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := db.Exec("INSERT INTO tasks (id, data) VALUES (?, ?)", id, fmt.Sprintf("data-%d", id))
			errCh <- err
		}(i)
	}

	wg.Wait()
	close(errCh)

	failures := 0
	for err := range errCh {
		if err != nil {
			failures++
		}
	}
	if failures > 0 {
		t.Errorf("%d/50 concurrent SQLite writes failed", failures)
	}

	// Verify all 50 rows were written
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 50 {
		t.Errorf("expected 50 rows, got %d", count)
	}
}
