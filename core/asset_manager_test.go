package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

type mockTaskStoreForAssets struct {
	store.ITaskStore
	exists bool
}

func (m *mockTaskStoreForAssets) Get(ctx context.Context, taskID, stepID string) (*store.StepResult, error) {
	if m.exists {
		return &store.StepResult{}, nil
	}
	return nil, os.ErrNotExist
}

func (m *mockTaskStoreForAssets) GetBatch(ctx context.Context, taskID string, stepIDs []string) (map[string]*store.StepResult, error) {
	result := make(map[string]*store.StepResult)
	for _, sid := range stepIDs {
		if r, err := m.Get(ctx, taskID, sid); err == nil {
			result[sid] = r
		}
	}
	return result, nil
}

func TestAssetManager_Handle(t *testing.T) {
	tempDir := t.TempDir()

	am := NewAssetManager(tempDir, 100, nil) // 100 bytes threshold

	t.Run("Small data stays inline", func(t *testing.T) {
		out, err := am.Handle("task1", "step1", []byte("small"), "txt", "")
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			t.Error("Expected nil OutputFile for small data")
		}
	})

	t.Run("Large data side-loads", func(t *testing.T) {
		largeData := make([]byte, 200)
		out, err := am.Handle("task1", "step1", largeData, "bin", "")
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			t.Fatal("Expected OutputFile for large data")
		}
		if _, err := os.Stat(out.Path); os.IsNotExist(err) {
			t.Errorf("File %s was not created", out.Path)
		}
	})
}

func TestAssetManager_SentryCleanup(t *testing.T) {
	// D-GC-1 fix (§4.1): orphan dirs are NOT auto-deleted in staging_only mode.
	// The old behavior (ts.Get with empty stepID → always error → os.RemoveAll)
	// was a P0 bug that deleted in-running task directories.
	tempDir := t.TempDir()

	am := NewAssetManager(tempDir, 10, nil)

	orphanDir := filepath.Join(tempDir, "orphan_task")
	os.MkdirAll(orphanDir, 0755)
	os.WriteFile(filepath.Join(orphanDir, "data.bin"), []byte("data"), 0644)

	// sweepOnce with nil TaskRegistry — should NOT delete the dir.
	stats := am.sweepOnce(context.Background(), time.Now(), nil)
	if stats.scanned != 1 {
		t.Errorf("expected scanned=1, got %d", stats.scanned)
	}

	if _, err := os.Stat(orphanDir); os.IsNotExist(err) {
		t.Error("orphan directory should NOT be deleted in staging_only mode (D-GC-1 fix)")
	}
}

// mockTaskRegistry implements store.TaskRegistry for sweepOnce tests.
type mockTaskRegistry struct {
	store.TaskRegistry
	row *store.TaskRow // nil = not-found, non-nil = return this row
	err error          // non-nil = return this error
}

func (m *mockTaskRegistry) LoadTask(ctx context.Context, taskID string) (*store.TaskRow, []byte, error) {
	return m.row, nil, m.err
}

// TestAssetSentry_LiveTaskNotDeleted verifies D-GC-1 fix (§6, defect 1):
// a running task's directory must survive a sweep. This is the P0 regression
// test — it precisely reproduces the old bug path where ts.Get(taskID, "")
// always failed and os.RemoveAll deleted the dir.
func TestAssetSentry_LiveTaskNotDeleted(t *testing.T) {
	tempDir := t.TempDir()

	am := NewAssetManager(tempDir, 10, nil)

	// Create a running task's output directory with a real artifact.
	taskDir := filepath.Join(tempDir, "task_running")
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, "report.md"), []byte("# Phase 3 Report"), 0644)

	// TaskRegistry says the task is running (non-terminal).
	tr := &mockTaskRegistry{row: &store.TaskRow{
		TaskID:    "task_running",
		Status:    "running",
		UpdatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}}

	stats := am.sweepOnce(context.Background(), time.Now().Add(2*time.Hour), tr)

	if _, err := os.Stat(filepath.Join(taskDir, "report.md")); os.IsNotExist(err) {
		t.Fatal("D-GC-1: running task's report.md was deleted by sweepOnce")
	}
	if stats.candidates > 0 {
		t.Errorf("running task should not be a deletion candidate, got candidates=%d", stats.candidates)
	}
}

// TestAssetSentry_StoreErrorKeepsFiles verifies D-GC-1 fix (§6, defect 1):
// when LoadTask returns a non-not-found error (DB busy, IO), the sweep must
// NOT treat it as "task doesn't exist" and delete the directory.
func TestAssetSentry_StoreErrorKeepsFiles(t *testing.T) {
	tempDir := t.TempDir()

	reg := &config.Registry{
		System: config.SystemSettings{
			Retention: config.RetentionSettings{AssetGC: "task_dirs"},
		},
	}
	am := NewAssetManager(tempDir, 10, reg)

	taskDir := filepath.Join(tempDir, "task_db_busy")
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, "data.json"), []byte("{}"), 0644)

	// TaskRegistry returns a real error (not not-found).
	tr := &mockTaskRegistry{err: fmt.Errorf("database is locked")}

	stats := am.sweepOnce(context.Background(), time.Now(), tr)

	if _, err := os.Stat(filepath.Join(taskDir, "data.json")); os.IsNotExist(err) {
		t.Fatal("D-GC-1: task dir deleted on DB error (should skip, not delete)")
	}
	if stats.skipped == 0 {
		t.Error("expected skipped=1 for DB error, got 0")
	}
}

// TestAssetSentry_NilRegistryKeepsFiles verifies D-GC-1 fix (§6, defect 1):
// when TaskRegistry is nil (persistence unavailable), the sweep must NOT
// delete anything. This is the safe default per the interface contract.
func TestAssetSentry_NilRegistryKeepsFiles(t *testing.T) {
	tempDir := t.TempDir()

	am := NewAssetManager(tempDir, 10, nil)

	taskDir := filepath.Join(tempDir, "task_unknown")
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, "output.txt"), []byte("result"), 0644)

	stats := am.sweepOnce(context.Background(), time.Now(), nil)

	if _, err := os.Stat(filepath.Join(taskDir, "output.txt")); os.IsNotExist(err) {
		t.Fatal("D-GC-1: task dir deleted with nil TaskRegistry (should skip)")
	}
	if stats.candidates > 0 {
		t.Errorf("nil registry should not produce candidates, got %d", stats.candidates)
	}
}

// TestAssetSentry_TaskDirsModeTerminalOldCandidate verifies §4.1 task_dirs mode:
// a terminal task older than AssetTTLDays is identified as a deletion candidate,
// but is NOT auto-deleted. In staging_only mode, it is not a candidate.
func TestAssetSentry_TaskDirsModeTerminalOldCandidate(t *testing.T) {
	tempDir := t.TempDir()

	// Create a terminal task dir with an old UpdatedAt.
	taskDir := filepath.Join(tempDir, "task_old_done")
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, "final.md"), []byte("done"), 0644)

	oldTime := time.Now().Add(-400 * 24 * time.Hour) // 400 days ago
	tr := &mockTaskRegistry{row: &store.TaskRow{
		TaskID:    "task_old_done",
		Status:    "completed",
		UpdatedAt: oldTime.Format("2006-01-02 15:04:05"),
	}}

	t.Run("task_dirs mode identifies candidate", func(t *testing.T) {
		reg := &config.Registry{
			System: config.SystemSettings{
				Retention: config.RetentionSettings{
					AssetGC:      "task_dirs",
					AssetTTLDays: 365,
				},
			},
		}
		am := NewAssetManager(tempDir, 10, reg)

		stats := am.sweepOnce(context.Background(), time.Now(), tr)
		if stats.candidates != 1 {
			t.Errorf("expected candidates=1, got %d", stats.candidates)
		}
		// File must still exist — background sweep does NOT auto-delete.
		if _, err := os.Stat(filepath.Join(taskDir, "final.md")); os.IsNotExist(err) {
			t.Fatal("task_dirs mode must NOT auto-delete (§3.3)")
		}
	})

	t.Run("staging_only mode no candidate", func(t *testing.T) {
		reg := &config.Registry{
			System: config.SystemSettings{
				Retention: config.RetentionSettings{
					AssetGC:      "staging_only",
					AssetTTLDays: 365,
				},
			},
		}
		am := NewAssetManager(tempDir, 10, reg)

		stats := am.sweepOnce(context.Background(), time.Now(), tr)
		if stats.candidates != 0 {
			t.Errorf("staging_only should not produce candidates, got %d", stats.candidates)
		}
	})
}

func TestAssetManager_HandleWithKind(t *testing.T) {
	tempDir := t.TempDir()

	am := NewAssetManager(tempDir, 10, nil)

	t.Run("Kind routes into subdirectory", func(t *testing.T) {
		largeData := make([]byte, 100)
		out, err := am.Handle("taskK", "step1", largeData, "json", "working")
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			t.Fatal("Expected OutputFile for large data")
		}
		// Path must be under <store>/<task>/working/
		rel, rerr := filepath.Rel(tempDir, out.Path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		want := filepath.Join("taskK", "working")
		if got := filepath.Dir(rel); got != want {
			t.Errorf("expected dir %q, got %q", want, got)
		}
	})

	t.Run("Empty kind stays flat", func(t *testing.T) {
		largeData := make([]byte, 100)
		out, err := am.Handle("taskF", "step1", largeData, "bin", "")
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(tempDir, out.Path)
		if got := filepath.Dir(rel); got != "taskF" {
			t.Errorf("expected flat dir taskF, got %q", got)
		}
	})
}

func TestAssetManager_EvidenceKindReadable(t *testing.T) {
	tempDir := t.TempDir()

	am := NewAssetManager(tempDir, 1, nil)
	_, err := am.Handle("taskE", "step1", []byte("raw evidence"), "txt", "evidence")
	if err != nil {
		t.Fatal(err)
	}

	// Evidence file must exist and be readable
	entries, _ := os.ReadDir(filepath.Join(tempDir, "taskE", "evidence"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 evidence file, got %d", len(entries))
	}
	raw, rerr := os.ReadFile(filepath.Join(tempDir, "taskE", "evidence", entries[0].Name()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(raw) != "raw evidence" {
		t.Errorf("evidence content mismatch: %q", string(raw))
	}
}
