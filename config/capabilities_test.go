package config

import (
	"testing"
)

func TestProviderConfig_Capabilities(t *testing.T) {
	t.Run("ExplicitCapability", func(t *testing.T) {
		pc := &ProviderConfig{
			Capabilities: []string{CapVision, CapT2I},
		}
		if !pc.HasCapability(CapVision) {
			t.Error("expected vision capability")
		}
		if !pc.HasCapability(CapT2I) {
			t.Error("expected t2i capability")
		}
		if pc.HasCapability(CapT2V) {
			t.Error("did not expect t2v capability")
		}
	})

	t.Run("SupportsVisionHeuristic", func(t *testing.T) {
		pcExplicit := &ProviderConfig{
			Capabilities: []string{CapVision},
		}
		if !pcExplicit.SupportsVisionHeuristic() {
			t.Error("expected SupportsVisionHeuristic to be true for explicit vision cap")
		}

		supportsVisionTrue := true
		pcDeprecated := &ProviderConfig{
			SupportsVision: &supportsVisionTrue,
		}
		if !pcDeprecated.SupportsVisionHeuristic() {
			t.Error("expected SupportsVisionHeuristic to be true for deprecated SupportsVision=true")
		}

		pcModel := &ProviderConfig{
			Model: "gpt-4o",
		}
		if !pcModel.SupportsVisionHeuristic() {
			t.Error("expected SupportsVisionHeuristic to be true for gpt-4o model name")
		}
	})
}

func TestRegistry_EnvCapabilities(t *testing.T) {
	r := &Registry{
		EnvCapabilities: []string{"python", "node"},
	}
	if !r.HasEnvCapability("python") {
		t.Error("expected python env capability")
	}
	if !r.HasEnvCapability("node") {
		t.Error("expected node env capability")
	}
	if r.HasEnvCapability("docker") {
		t.Error("did not expect docker env capability")
	}
}
