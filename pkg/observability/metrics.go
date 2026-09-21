package observability

import (
	"sync"
)

// Metrics manages production-grade counters and histograms.
type Metrics struct {
	mu       sync.RWMutex
	counters map[string]int64
	gauges   map[string]float64
}

var globalMetrics = &Metrics{
	counters: make(map[string]int64),
	gauges:   make(map[string]float64),
}

func GetGlobalMetrics() *Metrics {
	return globalMetrics
}

func (m *Metrics) Inc(name string, delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[name] += delta
}

func (m *Metrics) Set(name string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = value
}

func (m *Metrics) GetSnapshot() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]any)
	for k, v := range m.counters {
		out[k] = v
	}
	for k, v := range m.gauges {
		out[k] = v
	}
	return out
}
