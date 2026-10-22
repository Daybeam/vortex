package core

import (
	"sort"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
)

// provider_telemetry.go - Sliding window latency tracker + P95 calculator + 429 cooldown.
//
// Implements the telemetry layer for PROVIDER_ROUTING_TELEMETRY_DESIGN.md.

// ProviderMetrics holds sliding-window telemetry for one provider.
type ProviderMetrics struct {
	mu           sync.RWMutex
	latencies    []time.Duration
	windowSize   int
	errorCount   int64
	successCount int64
	last429Time  time.Time
}

// TelemetryRegistry holds metrics for all providers, keyed by provider name.
type TelemetryRegistry struct {
	mu      sync.RWMutex
	metrics map[string]*ProviderMetrics
	cfg     *config.ProviderRoutingConfig
}

// NewTelemetryRegistry creates a registry with the given config.
func NewTelemetryRegistry(cfg config.ProviderRoutingConfig) *TelemetryRegistry {
	if cfg.SlidingWindowSize <= 0 {
		cfg.SlidingWindowSize = 100
	}
	return &TelemetryRegistry{
		metrics: make(map[string]*ProviderMetrics),
		cfg:     &cfg,
	}
}

func (tr *TelemetryRegistry) getOrCreate(name string) *ProviderMetrics {
	tr.mu.RLock()
	m, ok := tr.metrics[name]
	tr.mu.RUnlock()
	if ok {
		return m
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if m, ok := tr.metrics[name]; ok {
		return m
	}
	m = &ProviderMetrics{
		latencies:  make([]time.Duration, 0, tr.cfg.SlidingWindowSize),
		windowSize: tr.cfg.SlidingWindowSize,
	}
	tr.metrics[name] = m
	return m
}

// RecordLatency records a request latency for the named provider.
func (tr *TelemetryRegistry) RecordLatency(name string, latency time.Duration) {
	m := tr.getOrCreate(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, latency)
	if len(m.latencies) > m.windowSize {
		m.latencies = m.latencies[1:]
	}
}

// RecordSuccess increments the success counter for the named provider.
func (tr *TelemetryRegistry) RecordSuccess(name string) {
	m := tr.getOrCreate(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.successCount++
}

// RecordError increments the error count for the named provider.
func (tr *TelemetryRegistry) RecordError(name string) {
	m := tr.getOrCreate(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errorCount++
}

// Record429 marks a 429 rate-limit event for the named provider.
func (tr *TelemetryRegistry) Record429(name string) {
	m := tr.getOrCreate(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last429Time = time.Now()
	m.errorCount++
}

// P95 returns the 95th percentile latency for the named provider.
func (tr *TelemetryRegistry) P95(name string) time.Duration {
	tr.mu.RLock()
	m, ok := tr.metrics[name]
	tr.mu.RUnlock()
	if !ok {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(m.latencies))
	copy(sorted, m.latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ErrorRate returns the error rate (0.0-1.0) for the named provider.
func (tr *TelemetryRegistry) ErrorRate(name string) float64 {
	tr.mu.RLock()
	m, ok := tr.metrics[name]
	tr.mu.RUnlock()
	if !ok {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := m.errorCount + m.successCount
	if total == 0 {
		return 0
	}
	return float64(m.errorCount) / float64(total)
}

// IsCoolingDown returns true if the provider received a 429 within the cooldown window.
func (tr *TelemetryRegistry) IsCoolingDown(name string) bool {
	cooldown := time.Duration(tr.cfg.CooldownSeconds) * time.Second
	if cooldown == 0 {
		cooldown = 60 * time.Second
	}
	tr.mu.RLock()
	m, ok := tr.metrics[name]
	tr.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.last429Time.IsZero() {
		return false
	}
	return time.Since(m.last429Time) < cooldown
}

// IsDegraded returns true if the provider's P95 exceeds the degrade threshold
// or its error rate exceeds 5%.
func (tr *TelemetryRegistry) IsDegraded(name string) bool {
	threshold := time.Duration(tr.cfg.P95DegradeThresholdMS) * time.Millisecond
	if threshold == 0 {
		threshold = 3 * time.Second
	}
	if tr.P95(name) > threshold {
		return true
	}
	return tr.ErrorRate(name) > 0.05
}
