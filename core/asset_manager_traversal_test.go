package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestAssetManager_Handle_RejectsTraversalIDs verifies that Handle rejects
// taskID/stepID values containing path separators or traversal sequences.
// This is the regression test for audit M3: asset_manager used taskID/stepID
// directly in filepath.Join without sanitization, allowing path traversal
// (e.g., taskID="../../etc" would write outside the asset store).
func TestAssetManager_Handle_RejectsTraversalIDs(t *testing.T) {
	tmpDir := t.TempDir()
	reg := &config.Registry{}
	am := NewAssetManager(tmpDir, 10, reg) // low threshold so any data triggers side-load

	data := []byte("this is test data that exceeds the 10 byte threshold")

	traversalIDs := []string{
		"../etc",
		"../../etc",
		"sub/../../etc",
		"a/b/c",
		"..",
		"foo\\bar", // backslash separator
	}

	for _, badID := range traversalIDs {
		// Test bad taskID
		_, err := am.Handle(badID, "step1", data, "txt", "")
		if err == nil {
			t.Errorf("Handle(taskID=%q) should return error but got nil", badID)
		}

		// Test bad stepID
		_, err = am.Handle("task1", badID, data, "txt", "")
		if err == nil {
			t.Errorf("Handle(stepID=%q) should return error but got nil", badID)
		}
	}
}

// TestAssetManager_Handle_AcceptsValidIDs verifies that Handle accepts normal
// taskID/stepID values without false positives (audit M3).
func TestAssetManager_Handle_AcceptsValidIDs(t *testing.T) {
	tmpDir := t.TempDir()
	reg := &config.Registry{}
	am := NewAssetManager(tmpDir, 10, reg)

	data := []byte("this is test data that exceeds the 10 byte threshold")

	validIDs := []string{
		"task-123",
		"step_abc",
		"task.with.dots",
		"task-with-dashes",
		"TASK_UPPER",
	}

	for _, validID := range validIDs {
		_, err := am.Handle(validID, "step1", data, "txt", "")
		if err != nil {
			t.Errorf("Handle(taskID=%q) should succeed but got error: %v", validID, err)
		}
	}
}
