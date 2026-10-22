package core

import (
	"testing"
)

func TestConstraintAdapter_Resolve(t *testing.T) {
	adapter := NewConstraintAdapter()

	// Test Case 1: Ollama/llama.cpp (Should return GBNF)
	schema := map[string]any{"type": "object", "properties": map[string]any{"price": map[string]any{"type": "number"}}}
	field, val := adapter.Resolve("ollama", schema)
	if field != "grammar" {
		t.Errorf("Expected field 'grammar', got %s", field)
	}
	if val == nil {
		t.Error("Expected GBNF grammar string, got nil")
	}

	// Test Case 2: vLLM (Should return guided_json)
	field, val = adapter.Resolve("vllm", schema)
	if field != "guided_json" {
		t.Errorf("Expected field 'guided_json', got %s", field)
	}
	if val == nil {
		t.Error("Expected schema, got nil")
	}

	// Test Case 3: OpenAI (Should return response_format)
	field, val = adapter.Resolve("openai", schema)
	if field != "response_format" {
		t.Errorf("Expected field 'response_format', got %s", field)
	}
}

// TestConstraintAdapter_ResolveErrorPaths covers error and edge paths that
// the happy-path test above does not exercise.
func TestConstraintAdapter_ResolveErrorPaths(t *testing.T) {
	adapter := NewConstraintAdapter()
	schema := map[string]any{"type": "object"}

	tests := []struct {
		name      string
		provider  string
		schema    map[string]any
		wantField string
		wantNil   bool
	}{
		{
			name:      "nil_schema_returns_empty",
			provider:  "ollama",
			schema:    nil,
			wantField: "",
			wantNil:   true,
		},
		{
			name:      "unknown_provider_returns_empty",
			provider:  "nonexistent_provider",
			schema:    schema,
			wantField: "",
			wantNil:   true,
		},
		{
			name:      "empty_provider_string",
			provider:  "",
			schema:    schema,
			wantField: "",
			wantNil:   true,
		},
		{
			name:      "nil_schema_with_unknown_provider",
			provider:  "unknown",
			schema:    nil,
			wantField: "",
			wantNil:   true,
		},
		{
			name:      "deepseek_maps_to_response_format",
			provider:  "deepseek",
			schema:    schema,
			wantField: "response_format",
			wantNil:   false,
		},
		{
			name:      "gemini_maps_to_response_schema",
			provider:  "gemini",
			schema:    schema,
			wantField: "response_schema",
			wantNil:   false,
		},
		{
			name:      "llama_cpp_maps_to_grammar",
			provider:  "llama.cpp",
			schema:    schema,
			wantField: "grammar",
			wantNil:   false,
		},
		{
			name:      "empty_schema_map_still_resolves",
			provider:  "vllm",
			schema:    map[string]any{},
			wantField: "guided_json",
			wantNil:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field, val := adapter.Resolve(tt.provider, tt.schema)
			if field != tt.wantField {
				t.Errorf("field = %q, want %q", field, tt.wantField)
			}
			if tt.wantNil && val != nil {
				t.Errorf("expected nil value, got %v", val)
			}
			if !tt.wantNil && val == nil {
				t.Errorf("expected non-nil value for provider %q", tt.provider)
			}
		})
	}
}
