package core

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSafeJoin_RejectsPathTraversal verifies that safeJoin rejects relative
// paths that escape the base directory. This is the regression test for
// audit M2: staged_workspace used filepath.Join without containment check,
// allowing path traversal (e.g., rel="../../etc/passwd") to write outside
// the task directory.
func TestSafeJoin_RejectsPathTraversal(t *testing.T) {
	base := "/tmp/test-task"

	// Normal relative paths should succeed.
	cases := []struct {
		rel string
		ok  bool
	}{
		{"file.txt", true},
		{"sub/file.txt", true},
		{"a/b/c.txt", true},
		{".", true},
		// Path traversal attempts — must fail.
		{"../etc/passwd", false},
		{"../../etc/passwd", false},
		{"../../../etc/passwd", false},
		{"sub/../../etc/passwd", false},
		// Absolute paths — must fail.
		{"/etc/passwd", false},
		// Empty is ok (resolves to base itself).
		{"", true},
	}

	for _, tc := range cases {
		_, err := safeJoin(base, tc.rel)
		if tc.ok && err != nil {
			t.Errorf("safeJoin(%q, %q) unexpected error: %v", base, tc.rel, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("safeJoin(%q, %q) expected error but got nil", base, tc.rel)
		}
	}

	// Windows-specific: drive letter paths must fail.
	if runtime.GOOS == "windows" {
		_, err := safeJoin(base, "C:\\Windows\\System32")
		if err == nil {
			t.Error("safeJoin should reject Windows drive-letter paths")
		}
	}
}

// TestStagedWorkspace_Rollback_RejectsTraversal verifies that Rollback rejects
// a tampered SnapshotIndex containing path traversal entries, rather than
// writing outside the task directory (audit M2).
func TestStagedWorkspace_Rollback_RejectsTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	sw := NewStagedWorkspace(tmpDir)

	// Create a sentinel file that we know does NOT exist outside tmpDir.
	sentinelName := fmt.Sprintf("sentinel_%d", time.Now().UnixNano())
	sentinelPath := filepath.Join(tmpDir, "..", "..", "..", "tmp", sentinelName)

	// Create a tampered snapshot with a path traversal entry.
	tampered := &SnapshotIndex{
		ID: "tampered",
		Files: map[string]StagingFileMeta{
			fmt.Sprintf("../../tmp/%s", sentinelName): {Path: fmt.Sprintf("../../tmp/%s", sentinelName), SHA256: "fake", Size: 4},
		},
	}

	// Rollback should return an error, not write outside tmpDir.
	err := sw.Rollback(tampered)
	if err == nil {
		t.Fatal("Rollback with path traversal should return error")
	}

	// Verify the traversal target was NOT created.
	if _, statErr := os.Stat(sentinelPath); statErr == nil {
		t.Fatalf("path traversal succeeded — sentinel file created outside task directory: %s (M2)", sentinelPath)
	}
}
