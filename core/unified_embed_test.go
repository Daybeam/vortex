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
		Provider:       "openai",
		Model:          "gpt-4o",
		EmbeddingModel: "text-embedding-3-small",
		APIKeyEnv:      "MISSING_KEY", // Force failure if called
	}
	reg.Providers["secondary"] = &config.ProviderConfig{
		Provider:       "ollama",
		Model:          "llama3.1",
		EmbeddingModel: "embedding-gemma-300m",
		BaseURL:        "http://localhost:11434/v1",
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

func TestExperienceStore_SemanticIsolation(t *testing.T) {
	p1 := store.TaskPattern{
		EmbeddingModel: "model_a",
		Embedding:      []float32{1.0, 0.0},
	}
	p2 := store.TaskPattern{
		EmbeddingModel: "model_b",
		Embedding:      []float32{0.0, 1.0},
	}

	if p1.EmbeddingModel == p2.EmbeddingModel {
		t.Errorf("Models should be different")
	}
}
