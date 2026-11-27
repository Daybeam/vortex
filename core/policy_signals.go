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

	// Coarse-layer reads: use familyKeyStats (stable under temperature
	// variance) instead of TaskPatterns with TaskType matching.
	// Threshold: SampleCount >= 3 && AvgConfidence >= confidenceThreshold
	// for verified families; SampleCount >= 5 for unverified families
	// (VerifiedSampleCount == 0). Unverified entries have no execution
	// evidence backing their success, so we require more samples before
	// trusting them for policy decisions.
	// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §3.1, §4, §5
	if c.expStore != nil {
		confThreshold := 0.75
		if c.capProfileStore != nil {
			// Could derive from config; for now use the standard fallback.
		}
		var sum float64
		var count int
		for _, fs := range c.expStore.GetFamilyStatsSnapshot() {
			// P2 auto-eviction: families with ≥3 consecutive attributable
			// failures are temporarily excluded from the decision surface.
			// A single success resets the streak. Memory-only state, so a
			// process restart gives all families a fresh chance.
			// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §5 (P2)
			if fs.ConsecutiveFailures >= 3 {
				continue
			}
			minSamples := 3
			if fs.VerifiedSampleCount == 0 {
				minSamples = 5
			}
			if fs.SampleCount < minSamples || fs.AvgConfidence < confThreshold {
				continue
			}
			for _, cap := range fs.Capabilities {
				if cap == role.BaseCapability {
					sum += fs.AvgTurnsUsed
					count++
					break
				}
			}
		}
		if count > 0 {
			signals.HistoricalAvgTurns = sum / float64(count)
		}

		// Skill recommendations for weak-model bias
		signals.RecommendedSkills = c.expStore.QuerySkillRecommendations(
			ctx, role.BaseCapability, 0.7)
	}

	// Theta and success rate from CapabilityProfileStore
	if c.capProfileStore != nil && role.BaseCapability != "" {
		profiles, err := c.capProfileStore.GetAllProfiles(ctx)
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
