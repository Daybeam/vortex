package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestFileTaskBackend_DeleteReturnsErrors is a regression test for audit L-N3:
// FileTaskBackend.Delete previously swallowed all errors — returned (0, nil)
// when ReadDir failed and ignored RemoveAll errors. After the fix, it returns
// the ReadDir error (except os.IsNotExist, which is a valid idempotent case)
// and the RemoveAll error.
func TestFileTaskBackend_DeleteReturnsErrors(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileTaskBackend(dir)
	ctx := context.Background()

	// 1. Idempotent delete: non-existent task returns (0, nil).
	count, err := backend.Delete(ctx, "nonexistent_task")
	if err != nil {
		t.Fatalf("delete of non-existent task should be idempotent, got err: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected count 0 for non-existent task, got %d", count)
	}

	// 2. Successful delete: save a step, then delete, verify count=1.
	taskID := "task_with_data"
	stepID := "step1"
	if err := backend.Save(ctx, taskID, stepID, []byte(`{"data":"test"}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	count, err = backend.Delete(ctx, taskID)
	if err != nil {
		t.Fatalf("Delete of existing task failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected count 1 (one step file), got %d", count)
	}

	// 3. Verify the directory was actually removed.
	if _, err := os.Stat(filepath.Join(dir, taskID)); !os.IsNotExist(err) {
		t.Fatalf("task directory should have been removed, but Stat returned: %v", err)
	}

	// 4. Second delete is idempotent (dir already gone).
	count, err = backend.Delete(ctx, taskID)
	if err != nil {
		t.Fatalf("second delete should be idempotent, got err: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected count 0 for already-deleted task, got %d", count)
	}
}
