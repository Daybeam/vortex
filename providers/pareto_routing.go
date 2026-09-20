package providers

import (
	"fmt"
	"sort"

	"github.com/daybeam/vortex/pkg/pareto"
)

// CapabilityProfileLookup provides per-model per-capability telemetry to the
// router for Pareto-aware selection. Implementations should be backed by
// store.CapabilityProfileStore.
type CapabilityProfileLookup interface {
	Lookup(modelID, capability string) (successRate, avgTokenCost, avgLatencyMs float64, found bool)
}

// paretoCandidate wraps a ProviderSlot with capability telemetry for Pareto
// frontier evaluation. Objectives: [SuccessRate (max), TokenCost (min), Latency (min)].
type paretoCandidate struct {
	slot        *ProviderSlot
	successRate float64
	tokenCost   float64
	latencyMs   float64
}

func (c paretoCandidate) Objectives() ([]float64, []bool) {
	return []float64{c.successRate, c.tokenCost, c.latencyMs}, []bool{false, true, true}
}

// NextPareto selects the best healthy provider using Pareto frontier filtering
// across [SuccessRate, TokenCost, Latency]. When profileLookup is nil or no
// candidates have telemetry, it falls back to the latency-minimizing Next().
//
// The capability parameter keys the profile lookup. If no profiles are found
// for any candidate, the method degrades gracefully to Next().
func (r *ProviderRouter) NextPareto(poolID, capability string, profileLookup CapabilityProfileLookup) (*ProviderSlot, error) {
	if profileLookup == nil {
		return r.Next(poolID)
	}

	r.mu.RLock()
	var healthySlots []*ProviderSlot
	for _, slot := range r.slots {
		if slot.PoolID == poolID && slot.IsHealthy() {
			healthySlots = append(healthySlots, slot)
		}
	}
	r.mu.RUnlock()

	if len(healthySlots) == 0 {
		return nil, fmt.Errorf("no healthy provider found for pool %q", poolID)
	}

	candidates := make([]paretoCandidate, 0, len(healthySlots))
	anyFound := false
	for _, slot := range healthySlots {
		modelID := slot.Config.Model
		successRate, tokenCost, latencyMs, found := profileLookup.Lookup(modelID, capability)
		if !found {
			slot.mu.RLock()
			latencyMs = slot.LatencyEMA
			slot.mu.RUnlock()
			successRate = 0.5
			tokenCost = 0
		} else {
			anyFound = true
		}
		if latencyMs == 0 {
			latencyMs = 1000
		}
		candidates = append(candidates, paretoCandidate{
			slot:        slot,
			successRate: successRate,
			tokenCost:   tokenCost,
			latencyMs:   latencyMs,
		})
	}

	if !anyFound {
		return r.Next(poolID)
	}

	frontier := pareto.FilterFrontier(candidates)
	if len(frontier) == 0 {
		return r.Next(poolID)
	}

	sort.Slice(frontier, func(i, j int) bool {
		if frontier[i].successRate != frontier[j].successRate {
			return frontier[i].successRate > frontier[j].successRate
		}
		return frontier[i].latencyMs < frontier[j].latencyMs
	})

	best := frontier[0].slot
	best.mu.Lock()
	if best.Status != StatusHealthy {
		best.Status = StatusHealthy
		best.FailCount = 0
	}
	best.mu.Unlock()
	return best, nil
}
