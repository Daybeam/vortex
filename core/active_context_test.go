package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/store"
)

type mockExpStore struct {
	store.IExperienceStore
	nodes      []*store.ExperienceNode
	lastBudget int // records the tokenBudget passed to RetrieveRelevantExperience
}

func (m *mockExpStore) RetrieveRelevantExperience(ctx context.Context, query []float32, capability, errorSignal string, tokenBudget int) []*store.ExperienceNode {
	m.lastBudget = tokenBudget
	return m.nodes
}

func (m *mockExpStore) QueryRelevantAntiPatterns(intent string, limit int) []store.AntiPatternPrecedent {
	return nil
}

type mockEmbedClient struct {
	store.IEmbeddingClient
}

func (m *mockEmbedClient) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	return []float32{0.1, 0.2}, "test-model", nil
}

func TestActiveContextAssembler_Assemble(t *testing.T) {
	mockStore := &mockExpStore{
		nodes: []*store.ExperienceNode{
			{
				Strategy:    "Failed attempt",
				Outcome:     "failure",
				ErrorSignal: "timeout",
			},
			{
				Strategy:   "Success strategy",
				Outcome:    "success",
				Confidence: 0.95,
			},
		},
	}
	mockEmbed := &mockEmbedClient{}

	assembler := NewActiveContextAssembler(mockStore, mockEmbed, 4000)

	prompt := assembler.Assemble(
		context.Background(),
		"Main task spec",
		"Step input data",
		"Previous error signal",
		"capability_x",
		"role_y",
	)

	// Verify Hot Context
	if !strings.Contains(prompt, "## Current Task\nMain task spec") {
		t.Errorf("Hot context (task) missing or incorrect")
	}
	if !strings.Contains(prompt, "## Current Step\nStep input data") {
		t.Errorf("Hot context (step) missing or incorrect")
	}
	if !strings.Contains(prompt, "## Previous Error\nPrevious error signal") {
		t.Errorf("Hot context (error) missing or incorrect")
	}

	// Verify Warm Context (Experience Graph)
	if !strings.Contains(prompt, "[LEARNED EXPERIENCE PRECEDENT]") {
		t.Errorf("Warm context header missing")
	}
	if !strings.Contains(prompt, "⚠️ Failure: Failed attempt → error: timeout") {
		t.Errorf("Failure node missing or incorrect")
	}
	if !strings.Contains(prompt, "✅ Success: Success strategy (confidence: 0.95)") {
		t.Errorf("Success node missing or incorrect")
	}
}

func TestActiveContextAssembler_Budgeting(t *testing.T) {
	// Test that the assembler passes the token budget to the store.
	mockStore := &mockExpStore{
		nodes: []*store.ExperienceNode{
			{Strategy: strings.Repeat("a", 10000), Outcome: "success"},
		},
	}
	assembler := NewActiveContextAssembler(mockStore, nil, 100) // Small budget

	prompt := assembler.Assemble(context.Background(), "task", "step", "", "cap", "role")

	if !strings.Contains(prompt, "## Current Task") {
		t.Errorf("Hot context should always be present")
	}
	// The assembler should pass warmBudget = TokenBudget/2 = 50 to the store.
	if mockStore.lastBudget != 50 {
		t.Errorf("expected store to receive budget 50 (TokenBudget/2), got %d", mockStore.lastBudget)
	}
}
