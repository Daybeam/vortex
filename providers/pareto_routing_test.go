package providers

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

type mockProvider struct {
	name string
}

func (m *mockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return nil, nil
}
func (m *mockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(text string) error) (*schemas.ProviderResponse, error) {
	return nil, nil
}
func (m *mockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *mockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *mockProvider) Name() string { return m.name }

type mockProfileLookup struct {
	profiles map[string]struct {
		successRate float64
		tokenCost   float64
		latencyMs   float64
	}
}

func (m *mockProfileLookup) Lookup(modelID, capability string) (float64, float64, float64, bool) {
	key := modelID + "|" + capability
	if p, ok := m.profiles[key]; ok {
		return p.successRate, p.tokenCost, p.latencyMs, true
	}
	return 0, 0, 0, false
}

func newTestRouter() *ProviderRouter {
	return &ProviderRouter{slots: make(map[string]*ProviderSlot)}
}

func registerTestSlot(r *ProviderRouter, id, poolID, model string, latency float64) {
	cfg := &config.ProviderConfig{Provider: id, Model: model, PoolID: poolID}
	slot := &ProviderSlot{
		ID:       id,
		Config:   cfg,
		PoolID:   poolID,
		Provider: &mockProvider{name: id},
		Status:   StatusHealthy,
	}
	slot.LatencyEMA = latency
	r.slots[id] = slot
}

func TestNextPareto_NilLookupFallsBackToNext(t *testing.T) {
	r := newTestRouter()
	registerTestSlot(r, "a", "pool1", "gpt-4o", 500)
	registerTestSlot(r, "b", "pool1", "gpt-4o-mini", 300)

	slot, err := r.NextPareto("pool1", "refactor", nil)
	if err != nil {
		t.Fatalf("NextPareto: %v", err)
	}
	if slot.ID != "b" {
		t.Errorf("expected lowest-latency slot b, got %s", slot.ID)
	}
}

func TestNextPareto_NoProfilesFallsBackToNext(t *testing.T) {
	r := newTestRouter()
	registerTestSlot(r, "a", "pool1", "gpt-4o", 500)
	registerTestSlot(r, "b", "pool1", "gpt-4o-mini", 300)

	lookup := &mockProfileLookup{profiles: map[string]struct {
		successRate float64
		tokenCost   float64
		latencyMs   float64
	}{}}

	slot, err := r.NextPareto("pool1", "refactor", lookup)
	if err != nil {
		t.Fatalf("NextPareto: %v", err)
	}
	if slot.ID != "b" {
		t.Errorf("expected fallback to Next (lowest latency), got %s", slot.ID)
	}
}

func TestNextPareto_SelectsParetoOptimal(t *testing.T) {
	r := newTestRouter()
	registerTestSlot(r, "flagship", "pool1", "gpt-4o", 2000)
	registerTestSlot(r, "light", "pool1", "gpt-4o-mini", 500)
	registerTestSlot(r, "bad", "pool1", "bad-model", 3000)

	lookup := &mockProfileLookup{profiles: map[string]struct {
		successRate float64
		tokenCost   float64
		latencyMs   float64
	}{
		"gpt-4o|refactor":      {0.95, 0.03, 2000},
		"gpt-4o-mini|refactor": {0.88, 0.005, 500},
		"bad-model|refactor":   {0.50, 0.04, 3000},
	}}

	slot, err := r.NextPareto("pool1", "refactor", lookup)
	if err != nil {
		t.Fatalf("NextPareto: %v", err)
	}
	if slot.ID == "bad" {
		t.Error("dominated 'bad' slot should never be selected")
	}
}

func TestNextPareto_PrefersHigherSuccessRateOnFrontier(t *testing.T) {
	r := newTestRouter()
	registerTestSlot(r, "a", "pool1", "model-a", 1000)
	registerTestSlot(r, "b", "pool1", "model-b", 800)

	lookup := &mockProfileLookup{profiles: map[string]struct {
		successRate float64
		tokenCost   float64
		latencyMs   float64
	}{
		"model-a|refactor": {0.95, 0.01, 1000},
		"model-b|refactor": {0.85, 0.01, 800},
	}}

	slot, err := r.NextPareto("pool1", "refactor", lookup)
	if err != nil {
		t.Fatalf("NextPareto: %v", err)
	}
	if slot.ID != "a" {
		t.Errorf("expected higher success rate slot a, got %s", slot.ID)
	}
}

func TestNextPareto_NoHealthyProviders(t *testing.T) {
	r := newTestRouter()
	cfg := &config.ProviderConfig{Provider: "x", Model: "m", PoolID: "pool1"}
	r.slots["x"] = &ProviderSlot{
		ID:       "x",
		Config:   cfg,
		PoolID:   "pool1",
		Provider: &mockProvider{name: "x"},
		Status:   StatusError,
	}
	r.slots["x"].LastErrorTime = time.Now()

	_, err := r.NextPareto("pool1", "refactor", nil)
	if err == nil {
		t.Error("expected error for no healthy providers")
	}
}

func TestNextPareto_PoolFilter(t *testing.T) {
	r := newTestRouter()
	registerTestSlot(r, "a", "pool1", "model-a", 500)
	registerTestSlot(r, "b", "pool2", "model-b", 100)

	lookup := &mockProfileLookup{profiles: map[string]struct {
		successRate float64
		tokenCost   float64
		latencyMs   float64
	}{
		"model-a|refactor": {0.9, 0.01, 500},
		"model-b|refactor": {0.95, 0.005, 100},
	}}

	slot, err := r.NextPareto("pool1", "refactor", lookup)
	if err != nil {
		t.Fatalf("NextPareto: %v", err)
	}
	if slot.ID != "a" {
		t.Errorf("expected slot a from pool1, got %s", slot.ID)
	}
}
