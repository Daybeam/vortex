package core

import (
	"github.com/daybeam/vortex/store"
	"testing"
)

type mockExpStoreForPotential struct {
	store.IExperienceStore
	potentials map[string]float64
}

func (m *mockExpStoreForPotential) GetStatePotential(stateHash string) float64 {
	if v, ok := m.potentials[stateHash]; ok {
		return v
	}
	return 1.0
}

func (m *mockExpStoreForPotential) QueryRelevantAntiPatterns(intent string, limit int) []store.AntiPatternPrecedent {
	return nil
}

func TestGenerateStateSignature(t *testing.T) {
	spawner := &Spawner{}

	res1 := map[string]any{"status": "ok"}
	sig1 := spawner.GenerateStateSignature("web_search", res1)

	res2 := map[string]any{"status": "error"}
	sig2 := spawner.GenerateStateSignature("web_search", res2)

	if sig1 == sig2 {
		t.Errorf("expected different signatures for different statuses, got %s and %s", sig1, sig2)
	}

	if sig1 == "" {
		t.Error("expected non-empty signature")
	}
}

func TestEvaluatePotentialDrop(t *testing.T) {
	mockExp := &mockExpStoreForPotential{
		potentials: map[string]float64{
			"state_ok":    0.9,
			"state_risky": 0.4,
		},
	}

	spawner := &Spawner{expStore: mockExp}

	// No drop
	alert, delta := spawner.EvaluatePotentialDrop("state_ok", "state_ok")
	if alert {
		t.Error("did not expect alert for same state")
	}

	// Significant drop (0.4 - 0.9 = -0.5)
	alert, delta = spawner.EvaluatePotentialDrop("state_ok", "state_risky")
	if !alert {
		t.Error("expected alert for significant drop")
	}
	if delta != -0.5 {
		t.Errorf("expected delta -0.5, got %f", delta)
	}
}
