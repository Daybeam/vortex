package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestPolicyDecider_IRT verifies the IRT PolicyDecider computes correct
// MaxTurns from signals. This test demonstrates the decoupling win:
// no need to mock IExperienceStore (30+ methods) or CapabilityProfileStore —
// just pass a PolicySignals struct directly.
func TestPolicyDecider_IRT(t *testing.T) {
	registry := &config.Registry{
		System: config.SystemSettings{MaxToolTurns: 50},
	}
	decider := NewIRTPolicyDecider(NewIRTBudgetEstimator(50), registry)

	signals := PolicySignals{
		HistoricalAvgTurns:    15.0,
		HistoricalSuccessRate: 0.8,
		Theta:                 0.5,
		RecommendedSkills:     []string{"skill_a", "skill_b"},
	}

	role := &config.Role{BaseCapability: "coding", MaxAdditionalSkills: 3}
	req := &SpawnRequest{Task: "write a function"}

	decision := decider.Decide(signals, req, role)

	if decision.MaxTurns < 50 {
		t.Errorf("expected MaxTurns >= 50 (static cap), got %d", decision.MaxTurns)
	}
	if len(decision.InjectedSkills) != 0 {
		t.Errorf("expected no skill injection for strong model (theta=0.5 >= -0.5), got %v", decision.InjectedSkills)
	}
}

// TestPolicyDecider_WeakModelSkillInjection verifies that weak models
// (theta < -0.5) get skill recommendations injected.
func TestPolicyDecider_WeakModelSkillInjection(t *testing.T) {
	registry := &config.Registry{
		System: config.SystemSettings{MaxToolTurns: 50},
	}
	decider := NewIRTPolicyDecider(NewIRTBudgetEstimator(50), registry)

	signals := PolicySignals{
		HistoricalAvgTurns:    10.0,
		HistoricalSuccessRate: 0.3,
		Theta:                 -0.8, // weak model
		RecommendedSkills:     []string{"proven_approach", "fallback_strategy"},
	}

	role := &config.Role{BaseCapability: "coding", MaxAdditionalSkills: 3}
	req := &SpawnRequest{Task: "debug a function"}

	decision := decider.Decide(signals, req, role)

	if len(decision.InjectedSkills) == 0 {
		t.Error("expected skill injection for weak model (theta=-0.8 < -0.5), got none")
	}
	if len(decision.InjectedSkills) > 3 {
		t.Errorf("expected at most 3 injected skills (MaxAdditionalSkills=3), got %d", len(decision.InjectedSkills))
	}
}

// TestPolicyDecider_NoDuplicateSkills verifies that skills already in
// req.AdditionalSkills are not duplicated.
func TestPolicyDecider_NoDuplicateSkills(t *testing.T) {
	registry := &config.Registry{
		System: config.SystemSettings{MaxToolTurns: 50},
	}
	decider := NewIRTPolicyDecider(NewIRTBudgetEstimator(50), registry)

	signals := PolicySignals{
		Theta:             -0.8, // weak model
		RecommendedSkills: []string{"existing_skill", "new_skill"},
	}

	role := &config.Role{BaseCapability: "coding", MaxAdditionalSkills: 3}
	req := &SpawnRequest{
		Task:             "test",
		AdditionalSkills: []string{"existing_skill"}, // already present
	}

	decision := decider.Decide(signals, req, role)

	for _, skill := range decision.InjectedSkills {
		if skill == "existing_skill" {
			t.Error("existing_skill was duplicated")
		}
	}
	if len(decision.InjectedSkills) != 1 {
		t.Errorf("expected 1 new skill injected (new_skill), got %d: %v", len(decision.InjectedSkills), decision.InjectedSkills)
	}
}

// TestSignalCollector_NilSafe verifies that the production SignalCollector
// handles nil stores gracefully (cold start).
func TestSignalCollector_NilSafe(t *testing.T) {
	collector := NewStoreBackedSignalCollector(nil, nil, nil)

	role := &config.Role{BaseCapability: "coding"}
	req := &SpawnRequest{Task: "test"}

	signals := collector.Collect(context.Background(), req, role)

	if signals.HistoricalAvgTurns != 0 {
		t.Errorf("expected 0 HistoricalAvgTurns with nil expStore, got %v", signals.HistoricalAvgTurns)
	}
	if signals.Theta != 0 {
		t.Errorf("expected 0 Theta with nil capProfileStore, got %v", signals.Theta)
	}
}

// TestCalculateMaxTurns_Glue verifies that calculateMaxTurns correctly
// composes SignalCollector + PolicyDecider and applies the decision.
func TestCalculateMaxTurns_Glue(t *testing.T) {
	// Create a spawner with test signal collector and decider
	s := &Spawner{
		signalCollector: &fixedSignalCollector{
			signals: PolicySignals{
				HistoricalAvgTurns:    20.0,
				HistoricalSuccessRate: 0.9,
				Theta:                 1.0, // strong model, no skill injection
			},
		},
		policyDecider: NewIRTPolicyDecider(
			NewIRTBudgetEstimator(50),
			&config.Registry{System: config.SystemSettings{MaxToolTurns: 50}},
		),
	}

	role := &config.Role{BaseCapability: "coding", MaxAdditionalSkills: 3}
	req := &SpawnRequest{Task: "write code", TurnsBudgetBonus: 5}

	maxTurns := s.calculateMaxTurns(context.Background(), req, role)

	if maxTurns < 55 {
		t.Errorf("expected maxTurns >= 55 (base + bonus=5), got %d", maxTurns)
	}
}

// fixedSignalCollector is a test SignalCollector that returns fixed signals.
// This demonstrates the testability win: no store mocking needed.
type fixedSignalCollector struct {
	signals PolicySignals
}

func (f *fixedSignalCollector) Collect(ctx context.Context, req *SpawnRequest, role *config.Role) PolicySignals {
	return f.signals
}
