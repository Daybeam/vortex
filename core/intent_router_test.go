package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// mockEmbeddingClient implements EmbeddingClient for testing without real network calls.
type mockEmbeddingClient struct {
	vec []float32
	err error
}

func (m *mockEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.vec, nil
}

func makeTestRegistry() *config.Registry {
	reg := &config.Registry{
		SOPs: make(map[string]*schemas.SOP),
	}
	reg.SOPs["code-review"] = &schemas.SOP{
		ID:          "code-review",
		Version:     "1.0",
		Description: "Perform code review and suggest improvements",
		Triggers:    []schemas.SOPTrigger{{Keywords: []string{"review", "code", "audit", "refactor"}}},
		Steps: map[string]schemas.SOPStep{
			"step-1": {
				ID:   "step-1",
				Role: "software_developer",
				Task: "Review the code and provide feedback",
			},
		},
	}
	return reg
}

func TestIntentRouter_Route_EmptyClient_Degraded(t *testing.T) {
	reg := makeTestRegistry()
	router := NewIntentRouter(nil)
	router.RefreshEmbeddings(context.Background(), reg)

	result, err := router.Route(context.Background(), reg, "review this code", 3)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if len(result) == 0 || result[0].SOP.ID != "code-review" {
		t.Errorf("Expected keyword fallback match with nil client, got %d results", len(result))
	}
}

func TestIntentRouter_Route_EmbeddingFails_KeywordFallback(t *testing.T) {
	reg := makeTestRegistry()
	client := &mockEmbeddingClient{err: context.DeadlineExceeded}
	router := NewIntentRouter(client)
	router.RefreshEmbeddings(context.Background(), reg)

	result, err := router.Route(context.Background(), reg, "Please review this code", 3)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	if len(result) == 0 {
		t.Fatalf("Expected keyword fallback to find code-review SOP")
	}

	if result[0].SOP.ID != "code-review" {
		t.Errorf("Expected SOP code-review, got %s (mode: %s)", result[0].SOP.ID, result[0].MatchMode)
	}
}
