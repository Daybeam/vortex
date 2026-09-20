package registry

import (
	"errors"
	"testing"
)

func TestNewModelRegistry_LoadsEmbedded(t *testing.T) {
	r := NewModelRegistry()
	embedded := r.ListEmbedded()
	if len(embedded) != 9 {
		t.Fatalf("expected 9 embedded models, got %d", len(embedded))
	}
}

func TestGetCapabilities_EmbeddedFallback(t *testing.T) {
	r := NewModelRegistry()
	caps := r.GetCapabilities("gpt-4o")
	if caps.MaxContextWindow != 128000 {
		t.Errorf("expected 128000, got %d", caps.MaxContextWindow)
	}
	if caps.MaxOutputTokens != 16384 {
		t.Errorf("expected 16384, got %d", caps.MaxOutputTokens)
	}
	if !caps.SupportsToolUse {
		t.Error("expected SupportsToolUse=true")
	}
	if caps.TierOverride != "flagship" {
		t.Errorf("expected flagship, got %s", caps.TierOverride)
	}
}

func TestGetCapabilities_CaseInsensitive(t *testing.T) {
	r := NewModelRegistry()
	caps := r.GetCapabilities("GPT-4O")
	if caps.MaxContextWindow != 128000 {
		t.Errorf("case-insensitive lookup failed: expected 128000, got %d", caps.MaxContextWindow)
	}
}

func TestGetCapabilities_SafeDefaults(t *testing.T) {
	r := NewModelRegistry()
	caps := r.GetCapabilities("unknown-model-xyz")
	if caps.MaxContextWindow != 8192 {
		t.Errorf("expected default 8192, got %d", caps.MaxContextWindow)
	}
	if caps.MaxOutputTokens != 4096 {
		t.Errorf("expected default 4096, got %d", caps.MaxOutputTokens)
	}
	if !caps.SupportsToolUse {
		t.Error("expected default SupportsToolUse=true")
	}
	if caps.ModelName != "unknown-model-xyz" {
		t.Errorf("expected model name preserved, got %s", caps.ModelName)
	}
}

func TestSetUserOverride_TakesPrecedence(t *testing.T) {
	r := NewModelRegistry()
	override := ModelCapabilities{
		ModelName:        "gpt-4o",
		MaxContextWindow: 999999,
		MaxOutputTokens:  999,
	}
	r.SetUserOverride(override)

	caps := r.GetCapabilities("gpt-4o")
	if caps.MaxContextWindow != 999999 {
		t.Errorf("user override not applied: expected 999999, got %d", caps.MaxContextWindow)
	}
	if caps.MaxOutputTokens != 999 {
		t.Errorf("user override not applied: expected 999, got %d", caps.MaxOutputTokens)
	}
}

func TestSetUserOverride_CaseInsensitive(t *testing.T) {
	r := NewModelRegistry()
	override := ModelCapabilities{
		ModelName:        "GPT-4O",
		MaxContextWindow: 500000,
	}
	r.SetUserOverride(override)

	caps := r.GetCapabilities("gpt-4o")
	if caps.MaxContextWindow != 500000 {
		t.Errorf("case-insensitive override failed: expected 500000, got %d", caps.MaxContextWindow)
	}
}

type mockExternalProvider struct {
	data map[string]ModelCapabilities
	err  error
}

func (m *mockExternalProvider) FetchLatestMetadata(modelName string) (*ModelCapabilities, error) {
	if m.err != nil {
		return nil, m.err
	}
	if cap, ok := m.data[modelName]; ok {
		return &cap, nil
	}
	return nil, errors.New("not found")
}

func TestSetExternalProvider_FetchedAndCached(t *testing.T) {
	r := NewModelRegistry()
	provider := &mockExternalProvider{
		data: map[string]ModelCapabilities{
			"custom-model": {
				ModelName:        "custom-model",
				MaxContextWindow: 64000,
				MaxOutputTokens:  8192,
			},
		},
	}
	r.SetExternalProvider(provider)

	caps := r.GetCapabilities("custom-model")
	if caps.MaxContextWindow != 64000 {
		t.Errorf("external provider not consulted: expected 64000, got %d", caps.MaxContextWindow)
	}

	caps2 := r.GetCapabilities("custom-model")
	if caps2.MaxContextWindow != 64000 {
		t.Errorf("runtime cache miss on second call: expected 64000, got %d", caps2.MaxContextWindow)
	}
}

func TestExternalProvider_ErrorFallsToDefaults(t *testing.T) {
	r := NewModelRegistry()
	provider := &mockExternalProvider{err: errors.New("network down")}
	r.SetExternalProvider(provider)

	caps := r.GetCapabilities("truly-unknown-model")
	if caps.MaxContextWindow != 8192 {
		t.Errorf("expected safe default on provider error, got %d", caps.MaxContextWindow)
	}
}

func TestExternalProvider_UserOverrideStillWins(t *testing.T) {
	r := NewModelRegistry()
	provider := &mockExternalProvider{
		data: map[string]ModelCapabilities{
			"gpt-4o": {
				ModelName:        "gpt-4o",
				MaxContextWindow: 111111,
			},
		},
	}
	r.SetExternalProvider(provider)
	r.SetUserOverride(ModelCapabilities{
		ModelName:        "gpt-4o",
		MaxContextWindow: 222222,
	})

	caps := r.GetCapabilities("gpt-4o")
	if caps.MaxContextWindow != 222222 {
		t.Errorf("user override should beat external provider: expected 222222, got %d", caps.MaxContextWindow)
	}
}

func TestExternalProvider_EmbeddedWinsOverRemote(t *testing.T) {
	r := NewModelRegistry()
	provider := &mockExternalProvider{
		data: map[string]ModelCapabilities{
			"gpt-4o": {
				ModelName:        "gpt-4o",
				MaxContextWindow: 111111,
			},
		},
	}
	r.SetExternalProvider(provider)

	caps := r.GetCapabilities("gpt-4o")
	if caps.MaxContextWindow != 128000 {
		t.Errorf("embedded should beat remote: expected 128000, got %d", caps.MaxContextWindow)
	}
}

func TestListEmbedded_ReturnsAllModels(t *testing.T) {
	r := NewModelRegistry()
	list := r.ListEmbedded()
	names := make(map[string]bool)
	for _, m := range list {
		names[m.ModelName] = true
	}
	expected := []string{
		"gpt-4o", "gpt-4o-mini", "gpt-4-turbo",
		"claude-3-5-sonnet-20241022", "claude-3-opus-20240229",
		"llama-3.1-8b-instruct", "llama-3.1-70b-instruct",
		"gemma-2-9b-it", "gemma-4-e4b-it",
	}
	for _, e := range expected {
		if !names[e] {
			t.Errorf("expected model %s in embedded list", e)
		}
	}
}
