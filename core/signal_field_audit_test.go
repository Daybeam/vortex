package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/types"
)

// TestSignalField_SensingReturnsSortedByStrength is the regression test for
// audit H1: Sensing must return task IDs sorted by signal intensity descending,
// not random map iteration order. Before the fix, the method returned the
// first `limit` keys from a Go map (random order).
func TestSignalField_SensingReturnsSortedByStrength(t *testing.T) {
	sf := NewSignalField(0.95, 10*time.Second)
	// Deposit different intensities for 3 tasks in the same layer.
	sf.Deposit(types.LayerCritical, "task_low", 0.1)
	sf.Deposit(types.LayerCritical, "task_high", 0.9)
	sf.Deposit(types.LayerCritical, "task_mid", 0.5)

	got := sf.Sensing(types.LayerCritical, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	// Must be sorted: high > mid > low
	if got[0] != "task_high" || got[1] != "task_mid" || got[2] != "task_low" {
		t.Fatalf("expected [task_high, task_mid, task_low], got %v", got)
	}
}

// TestSignalField_SensingGlobalReturnsSortedByEffectiveSignal is the
// regression test for audit H1: SensingGlobal must return task IDs sorted by
// effective signal descending, not random map iteration order.
func TestSignalField_SensingGlobalReturnsSortedByEffectiveSignal(t *testing.T) {
	sf := NewSignalField(0.95, 10*time.Second)
	// Place tasks in different layers with different weights.
	// Effective signal = sum(layer_weight * intensity) * (1 - agentLoad)
	// Weights: Critical=1.0, Dependency=0.6, Opportunistic=0.3
	sf.Deposit(types.LayerCritical, "task_a", 0.3)      // eff = 1.0 * 0.3 = 0.3
	sf.Deposit(types.LayerDependency, "task_b", 0.9)    // eff = 0.6 * 0.9 = 0.54
	sf.Deposit(types.LayerOpportunistic, "task_c", 0.9) // eff = 0.3 * 0.9 = 0.27

	got := sf.SensingGlobal(3, 0.0)
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	// Sorted by effective signal: b (0.54) > a (0.3) > c (0.27)
	if got[0] != "task_b" || got[1] != "task_a" || got[2] != "task_c" {
		t.Fatalf("expected [task_b, task_a, task_c] sorted by effective signal, got %v", got)
	}
}
