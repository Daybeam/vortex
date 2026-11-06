package core

import (
	"testing"

	"github.com/daybeam/vortex/pkg/types"
)

// audit M8 (2026-11-05): regression test for ApplyDecay with the
// snapshot-under-RLock approach. The behavior must be identical to the
// former hold-write-lock approach:
// 1. All signal values are multiplied by DecayRate.
// 2. Signals below 0.01 after decay are deleted.
// 3. Multiple layers are handled independently.

func TestApplyDecay_DecaysAndPrunes(t *testing.T) {
	sf := NewSignalField(0.5, 0) // 0 interval — we call ApplyDecay manually

	// Deposit signals across two layers.
	sf.Deposit(types.LayerCritical, "task_hot", 1.0)
	sf.Deposit(types.LayerCritical, "task_cold", 0.005) // 0.005 * 0.5 = 0.0025 < 0.01 → pruned
	sf.Deposit(types.LayerExploration, "task_explore", 0.8)

	// Apply decay once.
	sf.ApplyDecay()

	state := sf.GetState()

	// Check critical layer.
	critical, ok := state[string(types.LayerCritical)].(map[string]float64)
	if !ok {
		t.Fatalf("expected critical layer, got %T", state[string(types.LayerCritical)])
	}
	if got := critical["task_hot"]; got != 0.5 {
		t.Errorf("task_hot = %.4f, want 0.5 (1.0 * 0.5)", got)
	}
	if _, exists := critical["task_cold"]; exists {
		t.Error("task_cold should have been pruned (0.005 * 0.5 = 0.0025 < 0.01)")
	}

	// Check exploration layer.
	explore, ok := state[string(types.LayerExploration)].(map[string]float64)
	if !ok {
		t.Fatalf("expected exploration layer, got %T", state[string(types.LayerExploration)])
	}
	if got := explore["task_explore"]; got != 0.4 {
		t.Errorf("task_explore = %.4f, want 0.4 (0.8 * 0.5)", got)
	}
}

func TestApplyDecay_MultipleTicks(t *testing.T) {
	sf := NewSignalField(0.9, 0)

	sf.Deposit(types.LayerCritical, "task_a", 10.0)

	// Apply decay 3 times: 10.0 * 0.9^3 = 7.29
	sf.ApplyDecay()
	sf.ApplyDecay()
	sf.ApplyDecay()

	state := sf.GetState()
	critical := state[string(types.LayerCritical)].(map[string]float64)
	got := critical["task_a"]
	want := 10.0 * 0.9 * 0.9 * 0.9
	if got < want-0.001 || got > want+0.001 {
		t.Errorf("after 3 ticks: task_a = %.4f, want ~%.4f", got, want)
	}
}

func TestApplyDecay_EmptyLayersNoPanic(t *testing.T) {
	sf := NewSignalField(0.5, 0)
	// No deposits — all layers empty. Must not panic.
	sf.ApplyDecay()
	sf.ApplyDecay()
}
