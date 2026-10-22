package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

func TestTelemetryRegistry_P95(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{SlidingWindowSize: 10})

	for i := 0; i < 19; i++ {
		tr.RecordLatency("p1", 10*time.Millisecond)
	}
	tr.RecordLatency("p1", 100*time.Millisecond)

	p95 := tr.P95("p1")
	if p95 < 10*time.Millisecond || p95 > 100*time.Millisecond {
		t.Errorf("P95 should be between 10ms and 100ms, got %v", p95)
	}

	if tr.P95("unknown") != 0 {
		t.Error("P95 of unknown provider should be 0")
	}
}

func TestTelemetryRegistry_SlidingWindow(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{SlidingWindowSize: 5})

	for i := 0; i < 10; i++ {
		tr.RecordLatency("p1", time.Duration(i+1)*time.Millisecond)
	}

	p95 := tr.P95("p1")
	if p95 != 10*time.Millisecond {
		t.Errorf("P95 with sliding window 5 should be 10ms, got %v", p95)
	}
}

func TestTelemetryRegistry_ErrorRate(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{})

	tr.RecordSuccess("p1")
	tr.RecordSuccess("p1")
	tr.RecordError("p1")

	rate := tr.ErrorRate("p1")
	if rate != 1.0/3.0 {
		t.Errorf("error rate should be 1/3, got %v", rate)
	}
}

func TestTelemetryRegistry_Cooldown(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{CooldownSeconds: 60})

	if tr.IsCoolingDown("p1") {
		t.Error("provider with no 429 should not be cooling down")
	}

	tr.Record429("p1")
	if !tr.IsCoolingDown("p1") {
		t.Error("provider with recent 429 should be cooling down")
	}
}

func TestTelemetryRegistry_IsDegraded(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{P95DegradeThresholdMS: 50})

	if tr.IsDegraded("p1") {
		t.Error("provider with no data should not be degraded")
	}

	for i := 0; i < 10; i++ {
		tr.RecordLatency("p1", 100*time.Millisecond)
	}
	if !tr.IsDegraded("p1") {
		t.Error("provider with P95 > 50ms should be degraded")
	}
}

func TestProviderRouter_CoherenceFirst_StickyBinding(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{})
	router := NewProviderRouter(tr, "coherence_first")

	p1 := router.SelectProvider("task1", []string{"providerA", "providerB", "providerC"})
	if p1 == "" {
		t.Fatal("expected non-empty provider")
	}

	p2 := router.SelectProvider("task1", []string{"providerA", "providerB", "providerC"})
	if p2 != p1 {
		t.Errorf("coherence_first should be sticky: first=%q, second=%q", p1, p2)
	}
}

func TestProviderRouter_LatencyFirst_PicksLowestP95(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{SlidingWindowSize: 100})
	router := NewProviderRouter(tr, "latency_first")

	for i := 0; i < 10; i++ {
		tr.RecordLatency("fast", 10*time.Millisecond)
		tr.RecordSuccess("fast")
	}

	for i := 0; i < 10; i++ {
		tr.RecordLatency("slow", 100*time.Millisecond)
		tr.RecordSuccess("slow")
	}

	selected := router.SelectProvider("task1", []string{"fast", "slow"})
	if selected != "fast" {
		t.Errorf("latency_first should pick lowest P95 (fast), got %q", selected)
	}
}

func TestProviderRouter_LatencyFirst_SkipsCoolingDown(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{SlidingWindowSize: 100, CooldownSeconds: 60})
	router := NewProviderRouter(tr, "latency_first")

	tr.Record429("cooling")

	selected := router.SelectProvider("task1", []string{"cooling", "healthy"})
	if selected != "healthy" {
		t.Errorf("latency_first should skip cooling-down provider, got %q", selected)
	}
}

func TestProviderRouter_RecordResult(t *testing.T) {
	tr := NewTelemetryRegistry(config.ProviderRoutingConfig{SlidingWindowSize: 10})
	router := NewProviderRouter(tr, "coherence_first")

	router.RecordResult("p1", 50*time.Millisecond, nil)
	if tr.P95("p1") != 50*time.Millisecond {
		t.Errorf("expected P95=50ms after recording, got %v", tr.P95("p1"))
	}

	router.RecordResult("p1", 100*time.Millisecond, fmt.Errorf("test error"))
	if tr.ErrorRate("p1") != 0.5 {
		t.Errorf("expected error rate 0.5, got %v", tr.ErrorRate("p1"))
	}
}
