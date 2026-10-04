package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaSKG_TransitionRecording(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "caskg.json")

	manager := NewCaSKGManager(path)
	bus := NewEventBus()
	manager.HookEventBus(bus)

	// Simulate sequence: SkillA -> SkillB -> Success
	bus.Publish(NewAgentEvent("task-success", "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
	bus.Publish(NewAgentEvent("task-success", "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
	bus.Publish(NewAgentEvent("task-success", "", string(EventTaskCompleted), nil))

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	boost := manager.GetCausalBoost("SkillA", "SkillB")
	// Since TotalCount < 3, should be 1.0 (cold start)
	if boost != 1.0 {
		t.Errorf("expected 1.0 for cold start, got %f", boost)
	}

	// Add more successful transitions to cross threshold
	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("t-%d", i)
		bus.Publish(NewAgentEvent(tid, "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
		bus.Publish(NewAgentEvent(tid, "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
		bus.Publish(NewAgentEvent(tid, "", string(EventTaskCompleted), nil))
	}
	time.Sleep(200 * time.Millisecond)

	boost = manager.GetCausalBoost("SkillA", "SkillB")
	if boost <= 1.0 {
		t.Errorf("expected boost > 1.0 for successful sequence, got %f", boost)
	}

	// Simulate sequence: SkillA -> SkillC -> Failure
	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("tf-%d", i)
		bus.Publish(NewAgentEvent(tid, "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
		bus.Publish(NewAgentEvent(tid, "s2", string(EventStepCompleted), map[string]any{"skill": "SkillC"}))
		bus.Publish(NewAgentEvent(tid, "", string(EventTaskFailed), nil))
	}
	time.Sleep(200 * time.Millisecond)

	boost = manager.GetCausalBoost("SkillA", "SkillC")
	if boost >= 1.0 {
		t.Errorf("expected penalty < 1.0 for failing sequence, got %f", boost)
	}
}

// TestCaSKG_Save_AutoCreatesDir is a regression test for the bug where
// Save() failed with "no such file or directory" when the parent directory
// (e.g. data/) did not exist, causing causal learning to be silently lost
// across restarts. See: CaSKGManager Save() now calls os.MkdirAll.
func TestCaSKG_Save_AutoCreatesDir(t *testing.T) {
	// Use a deeply nested path that definitely doesn't exist.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "nested", "deep", "causal_skill_graph.json")

	// Sanity: the parent directory does not exist yet.
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: parent dir should not exist")
	}

	manager := NewCaSKGManager(path)

	// Record a transition so the graph has data to save.
	bus := NewEventBus()
	manager.HookEventBus(bus)
	bus.Publish(NewAgentEvent("t1", "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
	bus.Publish(NewAgentEvent("t1", "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
	bus.Publish(NewAgentEvent("t1", "", string(EventTaskCompleted), nil))
	time.Sleep(100 * time.Millisecond)

	// Save() should auto-create the directory and succeed.
	if err := manager.Save(); err != nil {
		t.Fatalf("Save() should auto-create parent dir, got error: %v", err)
	}

	// Verify the file was actually written.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("file should exist after Save(), but does not: %s", path)
	}

	// Verify we can reload the persisted graph.
	reloaded := NewCaSKGManager(path)
	if len(reloaded.Graph.Nodes) == 0 {
		t.Fatalf("reloaded graph should have nodes, but is empty")
	}
}
