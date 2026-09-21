package providers

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// fakeProvider is a minimal no-op Provider implementation for router_test.go.
type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	return &ProviderResponse{}, nil
}
func (fakeProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	return &ProviderResponse{}, nil
}
func (fakeProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (fakeProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }

// TestNext_DoesNotCrossContaminateUnrelatedSameTypeProviders is a direct
// regression test for the bug fixed 2026-07-19: two completely unrelated,
// standalone named ProviderConfigs (no Instances relationship) that happen
// to share the same Provider TYPE string (e.g. two different "openai"-type
// configs, such as deepseek-v4-flash and hf-GLM-5.2 in the real deployed
// config_windows.json) must NOT be selectable via each other's Next() call.
// Before the fix, Next(cfg.Provider) matched on the bare type string, so
// whichever of the two registered first would be silently reused for every
// subsequent request naming the other -- sending the wrong API key/base_url
// to a completely different logical provider. Discovered live via a
// hf-GLM-5.2 probe that came back with deepseek-v4-flash's leaked API key
// in the error body.
func TestNext_DoesNotCrossContaminateUnrelatedSameTypeProviders(t *testing.T) {
	cfgA := &config.ProviderConfig{Provider: "openai", BaseURL: "http://router-test-provider-a.invalid", PoolID: "provider-a"}
	cfgB := &config.ProviderConfig{Provider: "openai", BaseURL: "http://router-test-provider-b.invalid", PoolID: "provider-b"}

	GlobalRouter.Register("router-test-slot-a", cfgA, fakeProvider{})
	GlobalRouter.Register("router-test-slot-b", cfgB, fakeProvider{})

	slotA, err := GlobalRouter.Next(PoolIDFor(cfgA))
	if err != nil {
		t.Fatalf("Next(PoolIDFor(cfgA)) failed: %v", err)
	}
	if slotA.Config.BaseURL != cfgA.BaseURL {
		t.Errorf("Next(PoolIDFor(cfgA)) returned a slot for BaseURL %q, want %q (cross-contamination)", slotA.Config.BaseURL, cfgA.BaseURL)
	}

	slotB, err := GlobalRouter.Next(PoolIDFor(cfgB))
	if err != nil {
		t.Fatalf("Next(PoolIDFor(cfgB)) failed: %v", err)
	}
	if slotB.Config.BaseURL != cfgB.BaseURL {
		t.Errorf("Next(PoolIDFor(cfgB)) returned a slot for BaseURL %q, want %q (cross-contamination)", slotB.Config.BaseURL, cfgB.BaseURL)
	}
}

// TestNext_EmptyPoolID_FallsBackToCacheKey_StillNoCollision verifies the
// defensive fallback in Register (poolID = providerCacheKey(cfg) when
// cfg.PoolID is empty) still prevents collision between two unrelated
// configs of the same type, for any ProviderConfig constructed outside
// Registry.Load()/register_provider/RegisterConfiguredInstances that never
// got a PoolID assigned.
func TestNext_EmptyPoolID_FallsBackToCacheKey_StillNoCollision(t *testing.T) {
	cfgA := &config.ProviderConfig{Provider: "openai", BaseURL: "http://router-test-nopool-a.invalid"}
	cfgB := &config.ProviderConfig{Provider: "openai", BaseURL: "http://router-test-nopool-b.invalid"}

	GlobalRouter.Register("router-test-nopool-slot-a", cfgA, fakeProvider{})
	GlobalRouter.Register("router-test-nopool-slot-b", cfgB, fakeProvider{})

	slotA, err := GlobalRouter.Next(PoolIDFor(cfgA))
	if err != nil {
		t.Fatalf("Next(PoolIDFor(cfgA)) failed: %v", err)
	}
	if slotA.Config.BaseURL != cfgA.BaseURL {
		t.Errorf("got BaseURL %q, want %q", slotA.Config.BaseURL, cfgA.BaseURL)
	}
}
