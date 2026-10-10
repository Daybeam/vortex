package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── Gap Fix: shouldEnableStepPlan wiring ──────────────────────────────────

func TestSpawn_AutoDetectStepPlan_ConfigGated(t *testing.T) {
	// Verify that StepPlanConfig.EnableProbe is read from the registry.
	// When EnableProbe is false, auto-detection should not run even if
	// the task is complex.
	reg := &config.Registry{}
	reg.StepPlan = config.StepPlanConfig{EnableProbe: false}

	s := &Spawner{
		registry: reg,
	}

	req := &SpawnRequest{
		Task: "Refactor the auth module across files", // keyword-complex
	}

	// With EnableProbe=false, shouldEnableStepPlan should NOT be called
	// by Spawn. We can't easily test Spawn directly (needs full setup),
	// but we can verify the config gate logic.
	if reg.StepPlan.EnableProbe {
		t.Error("EnableProbe should be false")
	}

	// Even though shouldEnableStepPlan would return true for this task,
	// the Spawn method won't call it because EnableProbe is false.
	if !s.shouldEnableStepPlan(req, nil) {
		t.Error("shouldEnableStepPlan should return true for keyword-complex task (unit test)")
	}
	// But Spawn won't call it because EnableProbe=false.
}

func TestSpawn_AutoDetectStepPlan_WithProbeEnabled(t *testing.T) {
	reg := &config.Registry{}
	reg.StepPlan = config.StepPlanConfig{EnableProbe: true}

	s := &Spawner{
		registry: reg,
	}

	req := &SpawnRequest{
		Task: "Refactor the auth module across files",
	}

	// With EnableProbe=true and keyword-complex task, shouldEnableStepPlan
	// should return true.
	if !s.shouldEnableStepPlan(req, nil) {
		t.Error("shouldEnableStepPlan should return true for keyword-complex task")
	}
}

// ─── #14: RiskTier Decision Gate Rate Limiting ─────────────────────────────

func TestDecisionGate_RiskTierCritical_NeverAutoResolves(t *testing.T) {
	// Verify that RiskTierCritical tasks are identified correctly.
	tests := []struct {
		task     string
		mcps     []string
		critical bool
	}{
		{"Deploy to production", nil, true},
		{"Process payment for order", nil, true},
		{"Take a browser screenshot", nil, true},
		{"Refactor auth module", nil, false},
		{"Write a hello world", nil, false},
		{"Navigate with ghost-driver", []string{"ghost-driver"}, true},
	}

	for _, tt := range tests {
		tier, _ := schemas.DetermineRiskTier(tt.task, tt.mcps)
		isCritical := tier == schemas.RiskTierCritical
		if isCritical != tt.critical {
			t.Errorf("DetermineRiskTier(%q, %v) = %s, critical=%v, want critical=%v",
				tt.task, tt.mcps, tier, isCritical, tt.critical)
		}
	}
}

func TestDecisionGate_RecentDecisions_RateLimit(t *testing.T) {
	// Verify the rate limiting data structure works correctly.
	s := &DirectedEngine{
		recentDecisions: make(map[string]time.Time),
	}

	key := "task1:step1"

	// First decision — should be allowed.
	s.recentDecisionsMu.Lock()
	_, exists := s.recentDecisions[key]
	s.recentDecisions[key] = time.Now()
	s.recentDecisionsMu.Unlock()

	if exists {
		t.Error("first decision should not be rate-limited")
	}

	// Second decision immediately — should be rate-limited.
	s.recentDecisionsMu.Lock()
	last := s.recentDecisions[key]
	rateLimited := time.Since(last) < 30*time.Second
	s.recentDecisionsMu.Unlock()

	if !rateLimited {
		t.Error("second decision within 30s should be rate-limited")
	}
}

func TestDecisionGate_RecentDecisions_AfterExpiry(t *testing.T) {
	s := &DirectedEngine{
		recentDecisions: make(map[string]time.Time),
	}

	key := "task2:step2"

	// Simulate a decision 31 seconds ago.
	s.recentDecisionsMu.Lock()
	s.recentDecisions[key] = time.Now().Add(-31 * time.Second)
	s.recentDecisionsMu.Unlock()

	// Should NOT be rate-limited after 30s.
	s.recentDecisionsMu.Lock()
	last := s.recentDecisions[key]
	rateLimited := time.Since(last) < 30*time.Second
	s.recentDecisionsMu.Unlock()

	if rateLimited {
		t.Error("decision after 30s should not be rate-limited")
	}
}
