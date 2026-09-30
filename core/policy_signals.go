package core

import (
	"context"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// ── Policy Signal Collection ────────────────────────────────────────────────
//
// Separates "where data comes from" (SignalCollector) from "what to do with it"
// (PolicyDecider). See docs/completed/2026-09-28/ (spawner_policy refactoring).
//
// Before: spawner_policy.go mixed data collection (expStore/capProfileStore
// queries), computation (IRT formula), and application (req mutation) in one
// file. Now: SignalCollector gathers data → PolicyDecider computes decision →
// caller applies it.

// PolicySignals is the data needed to make an adaptive policy decision.
type PolicySignals struct {
	HistoricalAvgTurns    float64
	HistoricalSuccessRate float64
	Theta                 float64
	RecommendedSkills     []string
}

// SignalCollector abstracts data collection for policy decisions.
// Production impl reads from expStore + capProfileStore; test impl returns
// fixed values without mocking IExperienceStore's 30+ methods.
type SignalCollector interface {
	Collect(ctx context.Context, req *SpawnRequest, role *config.Role) PolicySignals
}

// storeBackedSignalCollector is the production implementation that reads
// from the ExperienceStore and CapabilityProfileStore.
type storeBackedSignalCollector struct {
	expStore        store.IExperienceStore
	capProfileStore *store.CapabilityProfileStore
	irtEstimator    *IRTBudgetEstimator
}

// NewStoreBackedSignalCollector creates a SignalCollector backed by the
// experience store and capability profile store.
func NewStoreBackedSignalCollector(expStore store.IExperienceStore, capProfileStore *store.CapabilityProfileStore, irtEstimator *IRTBudgetEstimator) SignalCollector {
	return &storeBackedSignalCollector{
		expStore:        expStore,
		capProfileStore: capProfileStore,
		irtEstimator:    irtEstimator,
	}
}

func (c *storeBackedSignalCollector) Collect(ctx context.Context, req *SpawnRequest, role *config.Role) PolicySignals {
	signals := PolicySignals{}

	// Historical average turns from TaskPatterns
	if c.expStore != nil {
		var sum float64
		var count int
		for _, p := range c.expStore.GetTaskPatternsSnapshot() {
			if p.TaskType == role.BaseCapability && p.AvgTurnsUsed > 0 {
				sum += p.AvgTurnsUsed
				count++
			}
		}
		if count > 0 {
			signals.HistoricalAvgTurns = sum / float64(count)
		}

		// Skill recommendations for weak-model bias
		signals.RecommendedSkills = c.expStore.QuerySkillRecommendations(
			ctx, role.BaseCapability, 0.7) // audit PERF-NEW-1: was context.Background() — couldn't be cancelled
	}

	// Theta and success rate from CapabilityProfileStore
	if c.capProfileStore != nil && role.BaseCapability != "" {
		profiles, err := c.capProfileStore.GetAllProfiles(ctx) // audit PERF-NEW-1: was context.Background()
		if err == nil && len(profiles) > 0 {
			var thetaSum, successSum float64
			var n int
			for _, p := range profiles {
				if p.Capability == role.BaseCapability && p.TotalRuns > 0 {
					thetaSum += p.Theta
					successSum += p.SuccessRate
					n++
				}
			}
			if n > 0 {
				signals.Theta = thetaSum / float64(n)
				signals.HistoricalSuccessRate = successSum / float64(n)
			}
		}
	}

	return signals
}
