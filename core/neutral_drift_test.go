package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/types"
)

func TestSignalField_ExplorationLayer(t *testing.T) {
	sf := NewSignalField(0.95, 10*time.Second)

	// 1. Verify initialization
	if _, ok := sf.Layers[types.LayerExploration]; !ok {
		t.Fatal("LayerExploration not initialized in NewSignalField")
	}

	// 2. Verify Deposit and Sensing
	taskID := "exploratory_task_1"
	sf.Deposit(types.LayerExploration, taskID, 1.0)

	sensing := sf.Sensing(types.LayerExploration, 10)
	found := false
	for _, id := range sensing {
		if id == taskID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("task %s not found in exploration layer sensing", taskID)
	}

	// 3. Verify weighting in GetEffectiveSignal
	// Weights: Critical=1.0, Dependency=0.6, Opportunistic=0.3, Exploration=0.15
	effective := sf.GetEffectiveSignal(taskID, 0.0)
	expected := 1.0 * 0.15
	if effective != expected {
		t.Errorf("GetEffectiveSignal weight mismatch: got %.2f, want %.2f", effective, expected)
	}

	// 4. Verify decay
	sf.DecayRate = 0.5
	sf.ApplyDecay()
	intensity := sf.Layers[types.LayerExploration][taskID].Value
	if intensity != 0.5 {
		t.Errorf("decay failed: got %.2f, want 0.50", intensity)
	}
}
