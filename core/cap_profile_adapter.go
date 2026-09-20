package core

import (
	"context"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
)

// capProfileLookup adapts store.CapabilityProfileStore to the
// providers.CapabilityProfileLookup interface expected by NextPareto.
type capProfileLookup struct {
	store *store.CapabilityProfileStore
}

// NewCapProfileLookup creates a providers.CapabilityProfileLookup backed by
// the given CapabilityProfileStore. Returns nil if store is nil.
func NewCapProfileLookup(cps *store.CapabilityProfileStore) providers.CapabilityProfileLookup {
	if cps == nil {
		return nil
	}
	return &capProfileLookup{store: cps}
}

func (l *capProfileLookup) Lookup(modelID, capability string) (successRate, avgTokenCost, avgLatencyMs float64, found bool) {
	profile, err := l.store.GetProfile(context.Background(), modelID, capability)
	if err != nil || profile == nil || profile.TotalRuns == 0 {
		return 0, 0, 0, false
	}
	return profile.SuccessRate, profile.AvgTokenCost, profile.AvgLatencyMs, true
}

// routeProvider selects a healthy provider slot using Pareto-aware routing
// when capability telemetry is available, falling back to latency-minimizing
// Next() otherwise.
func (s *Spawner) routeProvider(poolID, capability string) (*providers.ProviderSlot, error) {
	if s.capLookup != nil && capability != "" {
		return providers.GlobalRouter.NextPareto(poolID, capability, s.capLookup)
	}
	return providers.GlobalRouter.Next(poolID)
}

// avgThetaForCapability returns the mean IRT theta across all models that have
// telemetry for the given capability. Returns 0 (neutral) if no profiles exist.
// Used as a pre-routing theta estimate for turn budgeting.
func (s *Spawner) avgThetaForCapability(capability string) float64 {
	if s.capProfileStore == nil || capability == "" {
		return 0
	}
	profiles, err := s.capProfileStore.GetAllProfiles(context.Background())
	if err != nil || len(profiles) == 0 {
		return 0
	}
	var sum float64
	var count int
	for _, p := range profiles {
		if p.Capability == capability && p.TotalRuns > 0 {
			sum += p.Theta
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// RecalculateThetas recomputes the IRT ability parameter (theta) for all
// (model, capability) profiles from their observed success rates via the Rasch
// model MLE, and persists the updated values. Call this periodically (e.g.
// every N task completions or on a timer) to keep theta estimates fresh.
// Best-effort: returns the number of profiles updated and any error.
func (s *Spawner) RecalculateThetas(ctx context.Context) (int, error) {
	if s.capProfileStore == nil {
		return 0, nil
	}
	profiles, err := s.capProfileStore.GetAllProfiles(ctx)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, p := range profiles {
		if p.TotalRuns == 0 {
			continue
		}
		theta := EstimateTheta(p.SuccessRate)
		if err := s.capProfileStore.UpdateTheta(ctx, p.ModelID, p.Capability, theta); err != nil {
			continue
		}
		updated++
	}
	return updated, nil
}
