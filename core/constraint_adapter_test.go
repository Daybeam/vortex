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
