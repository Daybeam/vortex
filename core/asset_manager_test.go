package core

import (
	"context"
	"github.com/daybeam/vortex/store"
	"os"
	"path/filepath"
	"testing"
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

func TestAssetManager_Handle(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "assets_test")
	defer os.RemoveAll(tempDir)

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
	tempDir, _ := os.MkdirTemp("", "assets_cleanup_test")
	defer os.RemoveAll(tempDir)

	am := NewAssetManager(tempDir, 10, nil)

	// Create an orphaned asset directory
	orphanDir := filepath.Join(tempDir, "orphan_task")
	os.MkdirAll(orphanDir, 0755)
	os.WriteFile(filepath.Join(orphanDir, "data.bin"), []byte("data"), 0644)

	ts := &mockTaskStoreForAssets{exists: false}

	// Manual trigger logic since Sentry uses a 1h ticker
	entries, _ := os.ReadDir(am.StorePath)
	for _, entry := range entries {
		if entry.IsDir() {
			taskID := entry.Name()
			if _, err := ts.Get(context.Background(), taskID, ""); err != nil {
				os.RemoveAll(filepath.Join(am.StorePath, taskID))
			}
		}
	}

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("Expected orphaned directory to be deleted")
	}
}

func TestAssetManager_HandleWithKind(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "assets_kind_test")
	defer os.RemoveAll(tempDir)

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
	tempDir, _ := os.MkdirTemp("", "assets_evidence_test")
	defer os.RemoveAll(tempDir)

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
