package extreme_tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
)

// TestExtreme_AssetManager_MultiDocument verifies that 10 large documents
// are side-loaded to disk without OOM, and asset:// references are returned.
//
// Extreme scenario: User processes 10 × 1MB documents simultaneously.
// Uses 1MB threshold (not 50MB) to keep test fast — mechanism is identical.
func TestExtreme_AssetManager_MultiDocument(t *testing.T) {
	tmpDir := t.TempDir()
	am := core.NewAssetManager(filepath.Join(tmpDir, "assets"), 1024, &config.Registry{})

	var refs []string
	for i := 0; i < 10; i++ {
		data := []byte(strings.Repeat("X", 2*1024)) // 2KB > 1KB threshold
		of, err := am.Handle(fmt.Sprintf("task-%d", i), "step-1", data, "txt", "evidence")
		if err != nil {
			t.Fatalf("doc %d: Handle failed: %v", i, err)
		}
		if of == nil {
			t.Fatalf("doc %d: expected OutputFile for data > threshold, got nil", i)
		}
		refs = append(refs, of.Path)

		// Verify file exists on disk
		if _, err := os.Stat(of.Path); err != nil {
			t.Fatalf("doc %d: asset file not on disk: %v", i, err)
		}
	}

	if len(refs) != 10 {
		t.Fatalf("expected 10 asset refs, got %d", len(refs))
	}
}

// TestExtreme_AssetManager_SuperLargeSingleDocument verifies a single
// 100MB document is side-loaded without loading entirely into memory
// for processing (the write path uses os.WriteFile which is fine).
//
// Extreme scenario: User generates a 100MB tool output.
func TestExtreme_AssetManager_SuperLargeSingleDocument(t *testing.T) {
	tmpDir := t.TempDir()
	am := core.NewAssetManager(filepath.Join(tmpDir, "assets"), 50*1024, &config.Registry{})

	// 100MB output — simulates a massive tool output (e.g., database dump)
	data := make([]byte, 100*1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	of, err := am.Handle("big-task", "dump-step", data, "bin", "output")
	if err != nil {
		t.Fatalf("Handle 100MB failed: %v", err)
	}
	if of == nil {
		t.Fatal("expected OutputFile for 100MB, got nil")
	}

	// Verify file size matches
	info, err := os.Stat(of.Path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Errorf("file size = %d, want %d", info.Size(), len(data))
	}
}

// TestExtreme_AssetManager_PathTraversalAttack verifies that crafted
// task IDs with path traversal sequences are rejected.
//
// Extreme scenario: Malicious user tries to escape asset store via "../"
func TestExtreme_AssetManager_PathTraversalAttack(t *testing.T) {
	tmpDir := t.TempDir()
	// threshold=1 so the size check (len(data) > threshold) passes and
	// validateID is reached. NewAssetManager converts 0 → 50KB default.
	am := core.NewAssetManager(filepath.Join(tmpDir, "assets"), 1, &config.Registry{})

	maliciousIDs := []string{
		"../../../etc/passwd",
		"..\\..\\..\\windows\\system32",
		"task/../../escape",
		"task/../other",
	}

	for _, id := range maliciousIDs {
		_, err := am.Handle(id, "step", []byte("data"), "txt", "")
		if err == nil {
			t.Errorf("expected error for malicious ID %q, got nil", id)
		}
	}
}
