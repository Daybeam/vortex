package core

import (
	"sync"
	"time"
)

// provider_router.go - Routing strategies: coherence_first (sticky) and latency_first (P95-optimal).
//
// Implements PROVIDER_ROUTING_TELEMETRY_DESIGN.md.
// coherence_first: pins a task to a provider for its lifecycle; retries same
// provider on error, falls over only on terminal failure or degradation.
// latency_first: selects provider per-step based on real-time P95 + error rate + 429 status.

// ProviderRouter makes provider selection decisions based on the configured strategy.
type ProviderRouter struct {
	mu        sync.RWMutex
	telemetry *TelemetryRegistry
	strategy  string
	sticky    map[string]string
}

// NewProviderRouter creates a router with the given telemetry registry and strategy.
func NewProviderRouter(telemetry *TelemetryRegistry, strategy string) *ProviderRouter {
	if strategy == "" {
		strategy = "coherence_first"
	}
	return &ProviderRouter{
		telemetry: telemetry,
		strategy:  strategy,
		sticky:    make(map[string]string),
	}
}

// SelectProvider chooses a provider for the given task from the available candidates.
// Returns the selected provider name, or "" if none are available.
func (r *ProviderRouter) SelectProvider(taskID string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	switch r.strategy {
	case "latency_first":
		return r.selectByTelemetry(candidates)
	default:
		return r.selectSticky(taskID, candidates)
	}
}

// selectSticky returns the bound provider for this task, or picks the first
// healthy candidate and binds it.
func (r *ProviderRouter) selectSticky(taskID string, candidates []string) string {
	r.mu.RLock()
	bound, ok := r.sticky[taskID]
	r.mu.RUnlock()

	if ok {
		for _, c := range candidates {
			if c == bound {
				if !r.telemetry.IsCoolingDown(bound) {
					return bound
				}
				break
			}
		}
	}

	for _, c := range candidates {
		if !r.telemetry.IsCoolingDown(c) {
			r.mu.Lock()
			r.sticky[taskID] = c
			r.mu.Unlock()
			return c
		}
	}

	return candidates[0]
}

// selectByTelemetry picks the provider with the lowest P95 latency that is not
// degraded or cooling down.
func (r *ProviderRouter) selectByTelemetry(candidates []string) string {
	type candidate struct {
		name string
		p95  time.Duration
	}

	var healthy []candidate
	for _, c := range candidates {
		if r.telemetry.IsCoolingDown(c) || r.telemetry.IsDegraded(c) {
			continue
		}
		healthy = append(healthy, candidate{name: c, p95: r.telemetry.P95(c)})
	}

	if len(healthy) == 0 {
		return candidates[0]
	}

	best := healthy[0]
	for _, h := range healthy[1:] {
		if h.p95 < best.p95 {
			best = h
		}
	}
	return best.name
}

// BindSession explicitly binds a task to a provider (coherence_first).
func (r *ProviderRouter) BindSession(taskID, providerName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sticky[taskID] = providerName
}

// UnbindSession removes the sticky binding for a task.
func (r *ProviderRouter) UnbindSession(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sticky, taskID)
}

// RecordResult is a convenience method that records latency and success/error
// for a provider in one call.
func (r *ProviderRouter) RecordResult(providerName string, latency time.Duration, err error) {
	if err != nil {
		r.telemetry.RecordError(providerName)
	} else {
		r.telemetry.RecordSuccess(providerName)
	}
	r.telemetry.RecordLatency(providerName, latency)
}

// Strategy returns the configured routing strategy.
func (r *ProviderRouter) Strategy() string {
	return r.strategy
}
