package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type precedentMockEmbeddingClient struct{}

func (m *precedentMockEmbeddingClient) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	// Simple mock: return a deterministic vector based on text
	vec := make([]float32, 4)
	if text == "test intent" || text == "test intent\n" {
		vec = []float32{1, 0, 0, 0}
	} else if text == "similar intent" {
		vec = []float32{0.9, 0.1, 0, 0}
	} else if text == "unrelated" {
		vec = []float32{0, 0, 1, 0}
	} else {
		vec = []float32{0, 0, 0, 1}
	}
	return vec, "mock-model", nil
}

func TestExperienceStore_DecisionPrecedents(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es, _ := store.NewExperienceStore(tmpDir, ts, &config.SystemSettings{}, nil, nil)

	client := &precedentMockEmbeddingClient{}
	es.SetEmbeddingClient(client)

	node1 := &schemas.DecisionNode{
		ID:        "dec1",
		Reasoning: "I should do X because of Y",
		Action:    "do_x",
		Outcome:   "success",
		Timestamp: time.Now(),
	}

	// 1. Upsert
	err := es.UpsertDecisionPrecedent(context.Background(), node1)
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	// Wait for async embedding
	time.Sleep(100 * time.Millisecond)

	node2 := &schemas.DecisionNode{
		ID:        "dec2",
		Reasoning: "test intent",
		Action:    "action2",
		Outcome:   "ok",
		Timestamp: time.Now(),
	}
	es.UpsertDecisionPrecedent(context.Background(), node2)
	time.Sleep(100 * time.Millisecond)

	results := es.QueryDecisionPrecedents(context.Background(), "similar intent", 5)
	if len(results) == 0 {
		t.Fatal("expected at least one precedent")
	}
	if results[0].ID != "dec2" {
		t.Errorf("expected dec2, got %s", results[0].ID)
	}

	// 3. Query Unrelated
	results = es.QueryDecisionPrecedents(context.Background(), "unrelated", 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results for unrelated query, got %d", len(results))
	}
}

func TestSpawner_PrecedentInjection(t *testing.T) {
	reg := &config.Registry{}
	ts := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	spawner := NewSpawner(reg, ts, nil, logger, nil, "outputs")

	precedents := []*schemas.DecisionNode{
		{
			ID:        "p1",
			Reasoning: "reason 1",
			Action:    "action 1",
			Outcome:   "ok",
		},
	}

	blocks, err := spawner.buildSystemPrompt(nil, &config.Role{Name: "test"}, nil, "text", &config.ProviderConfig{}, []string{}, nil, nil, false, precedents, "test task", "")
	if err != nil {
		t.Fatalf("buildSystemPrompt failed: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "# Past Decision Precedents") {
			found = true
			break
		}
	}

	if !found {
		t.Error("Past Decision Precedents block not found in system prompt")
	}
}
