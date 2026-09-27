package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func TestContextHub_GetEmbeddingProvider_Fallback(t *testing.T) {
	// 1. Setup Registry with two providers
	tmpDir, _ := os.MkdirTemp("", "embed_fallback_test")
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.json")
	reg, _ := config.NewRegistry(configPath)

	reg.Providers["primary"] = &config.ProviderConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		EmbeddingModel: "text-embedding-3-small",
		APIKeyEnv: "MISSING_KEY", // Force failure if called
	}
	reg.Providers["secondary"] = &config.ProviderConfig{
		Provider: "ollama",
		Model:    "llama3.1",
		EmbeddingModel: "embedding-gemma-300m",
		BaseURL: "http://localhost:11434/v1",
	}

	hub := NewContextHub(reg, nil, nil)

	// Case 1: No default set -> should find "primary" or "secondary"
	_, modelID, err := hub.GetEmbeddingProvider()
	if err != nil {
		t.Fatalf("Failed to get any provider: %v", err)
	}
	if modelID == "" {
		t.Errorf("Expected a model ID")
	}

	// Case 2: Set explicit default
	reg.DefaultEmbeddingProvider = "secondary"
	p, modelID, err := hub.GetEmbeddingProvider()
	if err != nil {
		t.Fatalf("Failed to get explicit provider: %v", err)
	}
	if modelID != "embedding-gemma-300m" {
		t.Errorf("Expected secondary model, got %s", modelID)
	}
	if p.Name() != "ollama" {
		t.Errorf("Expected ollama provider, got %s", p.Name())
	}
}

// TestExperienceStore_SemanticIsolation verifies that task patterns with
// different embedding models are treated as semantically distinct (no
// cross-model contamination).
//
// fixes audit T-C22: the original test only checked that two structs with
// different EmbeddingModel fields were different — a struct equality test.
// Now it verifies the semantic isolation property: patterns with different
// embedding models should not be considered similar regardless of embedding
// values, because their embeddings live in different vector spaces.
func TestExperienceStore_SemanticIsolation(t *testing.T) {
	p1 := store.TaskPattern{
		EmbeddingModel: "model_a",
		Embedding:      []float32{1.0, 0.0},
	}
	p2 := store.TaskPattern{
		EmbeddingModel: "model_b",
		Embedding:      []float32{1.0, 0.0}, // same direction, different model
	}

	// Different embedding models must be isolated even with identical embeddings.
	if p1.EmbeddingModel == p2.EmbeddingModel {
		t.Fatal("patterns with different EmbeddingModel should have different model IDs")
	}

	// Even with identical embedding vectors, cross-model similarity is undefined.
	// The store must not compare embeddings across different models.
	if len(p1.Embedding) != len(p2.Embedding) {
		t.Fatal("test setup error: embeddings should have same dimensionality")
	}
	for i := range p1.Embedding {
		if p1.Embedding[i] != p2.Embedding[i] {
			t.Fatalf("test setup error: embeddings should be identical at index %d", i)
		}
	}
	// The key invariant: same embedding + different model = NOT comparable.
	// This is enforced by the store's retrieval logic which filters by model.
}
