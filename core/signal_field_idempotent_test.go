package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/types"
)

// TestSignalField_StartIdempotent is the regression test for audit H2:
// Start() must be idempotent — calling it multiple times must not launch
// duplicate decay goroutines. Before the fix (sync.Once guard on Start),
// each Start() call launched a separate decayLoop goroutine. With N goroutines,
// each tick applies decay N times, causing the signal to decay N× faster than
// configured.
//
// We verify this by depositing a known value, calling Start() 3 times,
// waiting for exactly one decay tick, and checking the resulting value.
// With 1 goroutine (fixed): value = initial * decayRate = 1.0 * 0.5 = 0.5
// With 3 goroutines (bug):   value = initial * decayRate^3 = 1.0 * 0.125 = 0.125
// We assert value > 0.35, which is only satisfied by a single goroutine.
func TestSignalField_StartIdempotent(t *testing.T) {
	sf := NewSignalField(0.5, 80*time.Millisecond)

	// Deposit a known initial value.
	sf.Deposit(types.LayerCritical, "task_x", 1.0)

	// Call Start() 3 times — must launch only 1 decay goroutine total.
	sf.Start()
	sf.Start()
	sf.Start()
	defer sf.Stop()

	// Wait long enough for exactly 1 tick (80ms) but not 2 (160ms).
	// 120ms gives 50% buffer past the first tick.
	time.Sleep(120 * time.Millisecond)

	state := sf.GetState()
	layerState, ok := state[string(types.LayerCritical)].(map[string]float64)
	if !ok {
		t.Fatalf("expected critical layer as map[string]float64, got %T", state[string(types.LayerCritical)])
	}
	got, ok := layerState["task_x"]
	if !ok {
		t.Fatal("expected task_x in critical layer")
	}

	// With 1 goroutine after 1 tick: 1.0 * 0.5 = 0.5
	// With 3 goroutines after 1 tick: 1.0 * 0.5^3 = 0.125
	// Threshold 0.35 clearly separates the two cases.
	if got < 0.35 {
		t.Fatalf("value %.4f below threshold 0.35 — multiple decay goroutines are running (expected ~0.5 for single goroutine, got ~%.4f suggesting %d goroutines)",
			got, got, int(0.5/got))
	}
	t.Logf("value after 1 tick: %.4f (expected ~0.5 for single goroutine)", got)
}
