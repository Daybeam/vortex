package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestListGraphs_OrphanedAfterRestart is a regression test for Issue 1:
// after engine restart, tasks that were "running" or "blocked" when the
// engine crashed have no active goroutine, but their manifest.json still
// says "running". ListGraphs used to replay the stored status verbatim,
// showing "running" forever. The fix overrides to "orphaned".
//
// Before the fix: status == "running" (stale, no active process).
// After  the fix: status == "orphaned" (reflects reality).
func TestListGraphs_OrphanedAfterRestart(t *testing.T) {
	engine := newDelegationTestEngine(t)

	// Simulate a persisted task that was running when the engine crashed.
	taskID := "task_orphaned_test"
	manifestDir := filepath.Join(engine.outputBase, taskID)
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		t.Fatalf("failed to create manifest dir: %v", err)
	}

	graph := &schemas.TaskGraph{
		TaskID:    taskID,
		TraceID:   "trace_orphaned",
		Status:    schemas.GraphRunning, // was running when engine crashed
		Steps:     map[string]*schemas.Step{},
		CreatedAt: time.Now(),
	}
	manifestData, _ := json.Marshal(graph)
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), manifestData, 0644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	// The task is NOT in engine.graphs (simulating post-restart state).
	// ListGraphs should override "running" to "orphaned".
	tasks := engine.ListGraphs()
	var found bool
	for _, task := range tasks {
		if task["task_id"] == taskID {
			found = true
			status, _ := task["status"].(string)
			if status != "orphaned" {
				t.Errorf("expected orphaned status for persisted running task not in memory, got %q", status)
			}
			break
		}
	}
	if !found {
		t.Fatal("orphaned task not found in ListGraphs output")
	}
}

// TestListGraphs_CompletedPersistedNotOrphaned verifies that persisted tasks
// with terminal statuses (completed, failed, cancelled) are NOT overridden
// to orphaned — they are genuinely complete.
func TestListGraphs_CompletedPersistedNotOrphaned(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID := "task_completed_test"
	manifestDir := filepath.Join(engine.outputBase, taskID)
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		t.Fatalf("failed to create manifest dir: %v", err)
	}

	graph := &schemas.TaskGraph{
		TaskID:    taskID,
		TraceID:   "trace_completed",
		Status:    schemas.GraphCompleted,
		Steps:     map[string]*schemas.Step{},
		CreatedAt: time.Now(),
	}
	manifestData, _ := json.Marshal(graph)
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), manifestData, 0644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	tasks := engine.ListGraphs()
	for _, task := range tasks {
		if task["task_id"] == taskID {
			status, _ := task["status"].(string)
			if status != "completed" {
				t.Errorf("expected completed status to be preserved, got %q", status)
			}
			return
		}
	}
	t.Fatal("completed task not found in ListGraphs output")
}
