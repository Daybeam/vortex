package providers

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestSanitizeGeminiSchema(t *testing.T) {
	inputJSON := `
	{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"exclusiveMinimum": 0
			},
			"options": {
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"timeout": { "type": "number" }
				}
			},
			"list": {
				"type": "array",
				"items": {
					"type": "object",
					"additionalProperties": true
				}
			}
		},
		"additionalProperties": false
	}`

	var input map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatalf("failed to unmarshal input: %v", err)
	}

	sanitized := sanitizeGeminiSchema(input)

	// Check if forbidden keys are removed
	forbidden := []string{
		"$schema", "additionalProperties", "exclusiveMinimum", "exclusiveMaximum",
		"default", "examples", "pattern", "minLength", "maxLength", "minItems", "maxItems", "uniqueItems",
	}

	var checkForbidden func(m map[string]any, path string)
	checkForbidden = func(m map[string]any, path string) {
		for _, f := range forbidden {
			if _, ok := m[f]; ok {
				t.Errorf("found forbidden key %q at %s", f, path)
			}
		}
		for k, v := range m {
			if next, ok := v.(map[string]any); ok {
				checkForbidden(next, path+"."+k)
			} else if arr, ok := v.([]any); ok {
				for i, item := range arr {
					if nextItem, ok := item.(map[string]any); ok {
						checkForbidden(nextItem, path+"."+k+"["+fmt.Sprintf("%d", i)+"]")
					}
				}
			}
		}
	}

	checkForbidden(sanitized, "root")

	// Check if allowed keys remain
	if sanitized["type"] != "object" {
		t.Errorf("expected type: object, got %v", sanitized["type"])
	}
}

func TestNewProvider_Branding(t *testing.T) {
	tests := []struct {
		provider string
		expected string
	}{
		{"openai", "openai"},
		{"deepseek", "deepseek"},
		{"sensenova", "sensenova"},
		{"sentimes", "sentimes"},
		{"together", "together"},
		{"groq", "groq"},
		{"gemini", "gemini"},
		{"gemma", "gemma"},
	}

	for _, tt := range tests {
		cfg := &config.ProviderConfig{
			Provider: tt.provider,
			Model:    "test-model",
		}
		p, err := newProvider(cfg, config.ExternalRuntimes{})
		if err != nil {
			t.Errorf("newProvider(%q) failed: %v", tt.provider, err)
			continue
		}
		if p.Name() != tt.expected {
			t.Errorf("newProvider(%q).Name() = %q; want %q", tt.provider, p.Name(), tt.expected)
		}
	}
}

func TestNewProvider_ProtocolOverride(t *testing.T) {
	cfg := &config.ProviderConfig{
		Provider: "my-custom-vendor",
		Protocol: "openai",
		Model:    "test-model",
	}
	p, err := newProvider(cfg, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("newProvider with protocol override failed: %v", err)
	}

	// The provider instance should be an OpenAIProvider (technical implementation)
	// but p.Name() should return the semantic brand name "my-custom-vendor"
	if p.Name() != "my-custom-vendor" {
		t.Errorf("expected provider name %q, got %q", "my-custom-vendor", p.Name())
	}

	// Verify it's actually an OpenAIProvider
	if _, ok := p.(*OpenAIProvider); !ok {
		t.Errorf("expected *OpenAIProvider, got %T", p)
	}
}
