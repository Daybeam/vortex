package core

import (
	"path/filepath"
	"testing"
	"time"
)

// TestFilterItemsByTask_ScopeAware verifies that filterItemsByTask respects
// the MemoryScope tier (memory tiering gap A, 2026-10-23):
//   - team/global items are always included (cross-task sharing)
//   - task/project/empty items are filtered by TaskID
func TestFilterItemsByTask_ScopeAware(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "task-item", TaskID: "taskA", Scope: ScopeTask, Intent: "task scoped"},
		{NodeID: "proj-item", TaskID: "taskA", Scope: ScopeProject, Intent: "project scoped"},
		{NodeID: "team-item", TaskID: "taskB", Scope: ScopeTeam, Intent: "team convention"},
		{NodeID: "global-item", TaskID: "taskC", Scope: ScopeGlobal, Intent: "global knowledge"},
		{NodeID: "legacy-item", TaskID: "taskA", Scope: "", Intent: "legacy no scope"},
		{NodeID: "other-task", TaskID: "taskB", Scope: ScopeTask, Intent: "other task"},
	}

	// Querying taskA should return: taskA's task/project/legacy items + team + global
	got := filterItemsByTask(items, "taskA")

	wantIDs := map[string]bool{
		"task-item":   true,  // ScopeTask, TaskID matches
		"proj-item":   true,  // ScopeProject, TaskID matches
		"team-item":   true,  // ScopeTeam, cross-task shared
		"global-item": true,  // ScopeGlobal, cross-task shared
		"legacy-item": true,  // empty scope, TaskID matches (backward compat)
		// "other-task" should NOT be included (ScopeTask, TaskID != taskA)
	}

	if len(got) != len(wantIDs) {
		t.Errorf("expected %d items, got %d", len(wantIDs), len(got))
	}

	gotIDs := make(map[string]bool)
	for _, it := range got {
		gotIDs[it.NodeID] = true
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("expected %q in results, not found", id)
		}
	}
	if gotIDs["other-task"] {
		t.Errorf("other-task (ScopeTask, different TaskID) should not be included")
	}
}

// TestFilterItemsByTask_BackwardCompat verifies that items with empty Scope
// (legacy data) are filtered by TaskID exactly as before the Scope feature.
func TestFilterItemsByTask_BackwardCompat(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "a", TaskID: "taskA"},
		{NodeID: "b", TaskID: "taskB"},
		{NodeID: "c", TaskID: "taskA"},
	}

	got := filterItemsByTask(items, "taskA")
	if len(got) != 2 {
		t.Fatalf("expected 2 items for taskA, got %d", len(got))
	}
	for _, it := range got {
		if it.TaskID != "taskA" {
			t.Errorf("expected only taskA items, got TaskID %s", it.TaskID)
		}
	}
}

// TestFilterItemsByTask_EmptyTaskScope verifies behavior when taskScope is empty.
// Team/global items should still be included; task-scoped items with empty
// TaskID would match (but that's an edge case of the existing behavior).
func TestFilterItemsByTask_EmptyTaskScope(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "task-item", TaskID: "taskA", Scope: ScopeTask},
		{NodeID: "team-item", TaskID: "taskB", Scope: ScopeTeam},
		{NodeID: "global-item", TaskID: "taskC", Scope: ScopeGlobal},
		{NodeID: "legacy-item", TaskID: "taskA", Scope: ""},
	}

	got := filterItemsByTask(items, "")

	// With empty taskScope, only team/global items pass (TaskID != "" for others)
	if len(got) != 2 {
		t.Fatalf("expected 2 items (team+global) for empty taskScope, got %d", len(got))
	}
	gotIDs := make(map[string]bool)
	for _, it := range got {
		gotIDs[it.NodeID] = true
	}
	if !gotIDs["team-item"] {
		t.Errorf("team-item should be included with empty taskScope")
	}
	if !gotIDs["global-item"] {
		t.Errorf("global-item should be included with empty taskScope")
	}
}

// TestSearchWithOptions_ScopeAwareRetrieval is an integration test verifying
// that SearchWithOptions returns team/global items even when searching with
// a different taskScope.
func TestSearchWithOptions_ScopeAwareRetrieval(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	// A team convention item belonging to a different task
	teamItem := MemoryItem{
		TaskID:    "task-other",
		NodeID:    "team-convention",
		Intent:    "always use parameterized queries",
		Summary:   "team SQL convention",
		Scope:     ScopeTeam,
		Timestamp: time.Now(),
	}
	// A task-scoped item for the current task
	taskItem := MemoryItem{
		TaskID:    "task-current",
		NodeID:    "task-result",
		Intent:    "always use parameterized queries",
		Summary:   "task specific finding",
		Scope:     ScopeTask,
		Timestamp: time.Now(),
	}
	// A task-scoped item for a different task (should NOT appear)
	otherTaskItem := MemoryItem{
		TaskID:    "task-other",
		NodeID:    "other-task-result",
		Intent:    "always use parameterized queries",
		Summary:   "other task finding",
		Scope:     ScopeTask,
		Timestamp: time.Now(),
	}

	if err := archive.Append(teamItem); err != nil {
		t.Fatalf("Append team: %v", err)
	}
	if err := archive.Append(taskItem); err != nil {
		t.Fatalf("Append task: %v", err)
	}
	if err := archive.Append(otherTaskItem); err != nil {
		t.Fatalf("Append other task: %v", err)
	}

	// Search from task-current — should see team-convention + task-result,
	// but NOT other-task-result.
	results := archive.Search("parameterized", nil, 10, "task-current", "")

	found := make(map[string]bool)
	for _, r := range results {
		found[r.NodeID] = true
	}

	if !found["team-convention"] {
		t.Errorf("team-convention (ScopeTeam) should be visible from task-current")
	}
	if !found["task-result"] {
		t.Errorf("task-result (ScopeTask, same task) should be visible")
	}
	if found["other-task-result"] {
		t.Errorf("other-task-result (ScopeTask, different task) should NOT be visible")
	}
}
