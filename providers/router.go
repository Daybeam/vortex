package providers

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
)

// ProviderStatus represents the health status of a provider slot.
type ProviderStatus int

const (
	StatusHealthy ProviderStatus = iota
	StatusRateLimited
	StatusError
)

// ProviderSlot holds a provider instance and its health metrics.
type ProviderSlot struct {
	ID             string
	Config         *config.ProviderConfig
	PoolID         string
	Provider       Provider
	Status         ProviderStatus
	LatencyEMA     float64 // Exponential Moving Average of latency in ms
	LastErrorTime  time.Time
	FailCount      int
	RateLimitUntil time.Time

	mu sync.RWMutex
}

func (s *ProviderSlot) IsHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Status == StatusRateLimited && time.Now().After(s.RateLimitUntil) {
		return true // Will be reset on next successful grab or check
	}
	if s.Status == StatusError && time.Since(s.LastErrorTime) > 30*time.Second {
		return true // Cooldown period for generic errors passed
	}
	return s.Status == StatusHealthy
}

func (s *ProviderSlot) ReportSuccess(latencyMs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusHealthy
	s.FailCount = 0
	if s.LatencyEMA == 0 {
		s.LatencyEMA = latencyMs
	} else {
		// 0.2 alpha for EMA
		s.LatencyEMA = (0.2 * latencyMs) + (0.8 * s.LatencyEMA)
	}
}

func (s *ProviderSlot) ReportRateLimit(retryAfterSeconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusRateLimited
	s.FailCount++
	s.LastErrorTime = time.Now()
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = 10 * s.FailCount // Fallback backoff
	}
	s.RateLimitUntil = time.Now().Add(time.Duration(retryAfterSeconds) * time.Second)
}

func (s *ProviderSlot) ReportError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FailCount++
	s.LastErrorTime = time.Now()
	// If it fails consistently, mark as error
	if s.FailCount >= 3 {
		s.Status = StatusError
	}
}

// ProviderRouter manages multiple provider slots and routes requests.
type ProviderRouter struct {
	slots map[string]*ProviderSlot
	mu    sync.RWMutex
}

var GlobalRouter = &ProviderRouter{
	slots: make(map[string]*ProviderSlot),
}

// Register adds or updates a provider slot.
func (r *ProviderRouter) Register(id string, cfg *config.ProviderConfig, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID := cfg.PoolID
	if poolID == "" {
		poolID = providerCacheKey(cfg)
	}
	r.slots[id] = &ProviderSlot{
		ID:       id,
		Config:   cfg,
		PoolID:   poolID,
		Provider: p,
		Status:   StatusHealthy,
	}
}

// Deregister removes a provider slot by ID. Used for test cleanup.
func (r *ProviderRouter) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.slots, id)
}

func (r *ProviderRouter) ReportSuccess(id string, latencyMs float64) {
	r.mu.RLock()
	slot, ok := r.slots[id]
	r.mu.RUnlock()
	if ok {
		slot.ReportSuccess(latencyMs)
	}
}

func (r *ProviderRouter) ReportRateLimit(id string, retryAfter int) {
	r.mu.RLock()
	slot, ok := r.slots[id]
	r.mu.RUnlock()
	if ok {
		slot.ReportRateLimit(retryAfter)
	}
}

func (r *ProviderRouter) ReportError(id string) {
	r.mu.RLock()
	slot, ok := r.slots[id]
	r.mu.RUnlock()
	if ok {
		slot.ReportError()
	}
}

// Next selects the best healthy provider slot in the given failover pool.
// poolID should be the caller's resolved config.ProviderConfig.PoolID (see
// providers.PoolIDFor). Prior to 2026-07-19 this matched on the bare
// Provider type string, which silently merged unrelated standalone named
// providers sharing that type into one pool.
func (r *ProviderRouter) Next(poolID string) (*ProviderSlot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *ProviderSlot
	minLatency := math.MaxFloat64

	for _, slot := range r.slots {
		if slot.PoolID != poolID {
			continue
		}

		if slot.IsHealthy() {
			slot.mu.RLock()
			lat := slot.LatencyEMA
			slot.mu.RUnlock()

			if lat < minLatency {
				minLatency = lat
				best = slot
			}
		}
	}

	if best != nil {
		// Reset state if it was in cooldown and just selected
		best.mu.Lock()
		if best.Status != StatusHealthy {
			best.Status = StatusHealthy
			best.FailCount = 0
		}
		best.mu.Unlock()
		return best, nil
	}

	// No healthy provider found
	return nil, fmt.Errorf("no healthy provider found for pool %q", poolID)
}

// LookupByConfig returns a pre-registered provider whose config matches the
// given cache key, or nil if none exists. Used by providers.Get to check for
// test mocks or pre-injected providers before creating a new one.
func (r *ProviderRouter) LookupByConfig(cacheKey string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, slot := range r.slots {
		if providerCacheKey(slot.Config) == cacheKey && slot.IsHealthy() {
			return slot.Provider
		}
	}
	return nil
}
