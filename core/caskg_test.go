package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaSKG_TransitionRecording(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "caskg_test")
	defer os.RemoveAll(tmpDir)
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
