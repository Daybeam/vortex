package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSQLiteTaskBackend(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "task_store_test")
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteTaskBackend(db)
	ctx := context.Background()

	taskID := "task-1"
	stepID := "step-1"
	data := []byte(`{"result": "ok"}`)

	// 1. Save
	if err := backend.Save(ctx, taskID, stepID, data); err != nil {
		t.Errorf("Save failed: %v", err)
	}

	// 2. Load
	loaded, err := backend.Load(ctx, taskID, stepID)
	if err != nil {
		t.Errorf("Load failed: %v", err)
	}
	if string(loaded) != string(data) {
		t.Errorf("Data mismatch: got %s, want %s", loaded, data)
	}

	// 3. Claim (should fail because it's already 'done')
	claimed, err := backend.Claim(ctx, taskID, stepID)
	if err != nil {
		t.Errorf("Claim failed: %v", err)
	}
	if claimed {
		t.Error("Claim should have failed for a 'done' step")
	}

	// 4. New step Claim (should succeed)
	task2 := "task-2"
	step2 := "step-2"

	claimed2, err := backend.Claim(ctx, task2, step2)
	if err != nil {
		t.Errorf("Claim 2 failed: %v", err)
	}
	if !claimed2 {
		t.Error("Claim 2 should have succeeded for new step")
	}

	// 5. Concurrent Claim
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	task3 := "task-3"
	step3 := "step-3"

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := backend.Claim(ctx, task3, step3)
			if ok {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Errorf("Concurrent Claim failed: got %d successes, want 1", successCount)
	}

	// 6. Delete
	count, err := backend.Delete(ctx, taskID)
	if err != nil {
		t.Errorf("Delete failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Delete count mismatch: got %d, want 1", count)
	}
}
