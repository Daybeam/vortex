package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// Regression (deletion-review-log.md 2026-10-24): Logger.write() built the log
// filename from ev.TaskID with no guard, so system-level events logged with
// task_id="" produced a nameless file "logs/<UTC-date>/.jsonl" (and, before that,
// were scattered across dates). Empty task IDs must now route to "_system.jsonl"
// and NEVER create a nameless ".jsonl".
//
// This test fails before the guard (no _system.jsonl; a ".jsonl" appears) and
// passes after it.
func TestLogger_EmptyTaskID_WritesSystemFile(t *testing.T) {
	dir := mustTempDir(t, "logsysfile")
	logger := mustNewLogger(t, dir, &config.SystemSettings{})

	logger.Log(EventTaskSubmitted, "", "", map[string]any{"k": "v"})

	date := time.Now().UTC().Format("2006-01-02")
	dayDir := filepath.Join(dir, date)
	systemFile := filepath.Join(dayDir, "_system.jsonl")
	namelessFile := filepath.Join(dayDir, ".jsonl")

	// The writer goroutine persists asynchronously — poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(systemFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(systemFile); err != nil {
		t.Fatalf("expected empty-taskID events to land in _system.jsonl, got: %v", err)
	}
	if _, err := os.Stat(namelessFile); err == nil {
		t.Errorf("nameless .jsonl was created for an empty TaskID — guard regressed")
	}
}
