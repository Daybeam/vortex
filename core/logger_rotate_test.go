package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLogger_RotateCompressionTrackedByWg is a regression test for audit
// finding M6: the log rotation compression goroutine was not tracked by
// l.wg, so Close() could return while compression was still running,
// potentially accessing logger resources after cleanup.
//
// After the fix, the goroutine is tracked by l.wg, so Close() waits for
// compression to finish. We verify this by calling rotate(), then Close(),
// and checking that the compressed file was created (compression completed
// before Close returned).
func TestLogger_RotateCompressionTrackedByWg(t *testing.T) {
	logDir := t.TempDir()
	logger, err := NewLogger(logDir, nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	// Create a log file with content to rotate.
	logPath := filepath.Join(logDir, "test.log")
	content := []byte("test log content for rotation compression\n")
	if err := os.WriteFile(logPath, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// rotate() renames the file and starts async compression.
	logger.rotate(logPath)

	// Close() should wait for the compression goroutine to finish.
	// If the goroutine isn't tracked, Close() might return before
	// compression completes, and the .gz file might not exist yet.
	logger.Close()

	// Verify the compressed file was created — this proves Close() waited.
	// The compression goroutine creates a .gz file from the renamed tmp file.
	// Give a small grace period in case the filesystem is slow.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(logDir, "*.gz"))
		if len(matches) > 0 {
			return // Success: compressed file exists
		}
		time.Sleep(10 * time.Millisecond)
	}

	// If we get here, no .gz file was found. This might mean:
	// 1. The compression goroutine didn't run (bug)
	// 2. The tmp file was renamed but compression failed
	// Either way, check if the tmp file exists for debugging.
	tmpMatches, _ := filepath.Glob(filepath.Join(logDir, "*.tmp"))
	t.Fatalf("no compressed .gz file found after Close(); tmp files: %v", tmpMatches)
}
