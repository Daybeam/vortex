package providers

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestSetGlobalFallback_SetsChain(t *testing.T) {
	ClearCache()
	t.Cleanup(ClearCache)

	chain := []config.ProviderConfig{
		{Provider: "openai", Model: "gpt-4"},
		{Provider: "anthropic", Model: "claude-3"},
	}
	SetGlobalFallback(chain)

	mu.Lock()
	gf := globalFallback
	mu.Unlock()

	if len(gf) != 2 {
		t.Fatalf("expected 2 fallback entries, got %d", len(gf))
	}
	if gf[0].Provider != "openai" {
		t.Errorf("first fallback provider = %q, want %q", gf[0].Provider, "openai")
	}
	if gf[1].Provider != "anthropic" {
		t.Errorf("second fallback provider = %q, want %q", gf[1].Provider, "anthropic")
	}
}

func TestSetGlobalFallback_NilClearsChain(t *testing.T) {
	ClearCache()
	t.Cleanup(ClearCache)

	SetGlobalFallback([]config.ProviderConfig{
		{Provider: "openai", Model: "gpt-4"},
	})
	SetGlobalFallback(nil)

	mu.Lock()
	gf := globalFallback
	mu.Unlock()

	if len(gf) != 0 {
		t.Fatalf("expected 0 fallback entries after nil, got %d", len(gf))
	}
}

func TestSetGlobalFallback_EmptyClearsChain(t *testing.T) {
	ClearCache()
	t.Cleanup(ClearCache)

	SetGlobalFallback([]config.ProviderConfig{
		{Provider: "openai", Model: "gpt-4"},
	})
	SetGlobalFallback([]config.ProviderConfig{})

	mu.Lock()
	gf := globalFallback
	mu.Unlock()

	if len(gf) != 0 {
		t.Fatalf("expected 0 fallback entries after empty, got %d", len(gf))
	}
}

func TestClearCache_ResetsGlobalFallback(t *testing.T) {
	SetGlobalFallback([]config.ProviderConfig{
		{Provider: "openai", Model: "gpt-4"},
	})
	ClearCache()

	mu.Lock()
	gf := globalFallback
	mu.Unlock()

	if len(gf) != 0 {
		t.Fatalf("expected 0 fallback entries after ClearCache, got %d", len(gf))
	}
}
