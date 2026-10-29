package config

import (
	"testing"
)

// TestFindProviderByModel_ExactMatch is the regression test for
// FindProviderByModel, the locked accessor that resolves a model name
// to a ProviderConfig. This is the foundation for the escalate_model
// silent-fallback fix (P0-2, 2026-10-28).
func TestFindProviderByModel_ExactMatch(t *testing.T) {
	reg := &Registry{
		Providers: map[string]*ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o"},
			"gemini": {Provider: "gemini", Model: "gemini-flash-lite-latest"},
		},
	}

	pc := reg.FindProviderByModel("gpt-4o")
	if pc == nil {
		t.Fatal("expected to find provider for model gpt-4o, got nil")
	}
	if pc.Provider != "openai" {
		t.Errorf("expected provider openai, got %q", pc.Provider)
	}
	if pc.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %q", pc.Model)
	}
}

// TestFindProviderByModel_NoMatch returns nil when no provider uses the
// given model name.
func TestFindProviderByModel_NoMatch(t *testing.T) {
	reg := &Registry{
		Providers: map[string]*ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o"},
		},
	}

	if pc := reg.FindProviderByModel("claude-3-opus"); pc != nil {
		t.Errorf("expected nil for unknown model, got %+v", pc)
	}
}

// TestFindProviderByModel_EmptyModel returns nil for empty input.
func TestFindProviderByModel_EmptyModel(t *testing.T) {
	reg := &Registry{
		Providers: map[string]*ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o"},
		},
	}

	if pc := reg.FindProviderByModel(""); pc != nil {
		t.Errorf("expected nil for empty model, got %+v", pc)
	}
}

// TestFindProviderByModel_DeterministicSelection verifies that when
// multiple providers share the same model, the one with the
// lexicographically smallest ID is returned for determinism.
func TestFindProviderByModel_DeterministicSelection(t *testing.T) {
	reg := &Registry{
		Providers: map[string]*ProviderConfig{
			"zeta":   {Provider: "openai", Model: "gpt-4o"},
			"alpha":  {Provider: "openai", Model: "gpt-4o", BaseURL: "https://a"},
			"middle": {Provider: "openai", Model: "gpt-4o"},
		},
	}

	pc := reg.FindProviderByModel("gpt-4o")
	if pc == nil {
		t.Fatal("expected non-nil result")
	}
	if pc.BaseURL != "https://a" {
		t.Errorf("expected deterministic selection of provider 'alpha' (smallest ID), got BaseURL=%q", pc.BaseURL)
	}
}
