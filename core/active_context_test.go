package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/store"
)

type mockExpStore struct {
	store.IExperienceStore
	nodes []*store.ExperienceNode
}

func (m *mockExpStore) RetrieveRelevantExperience(ctx context.Context, query []float32, capability, errorSignal string, tokenBudget int) []*store.ExperienceNode {
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
	// Test that we respect the budget (minimal tokens)
	mockStore := &mockExpStore{
		nodes: []*store.ExperienceNode{
			{Strategy: strings.Repeat("a", 10000), Outcome: "success"},
		},
	}
	assembler := NewActiveContextAssembler(mockStore, nil, 100) // Small budget

	prompt := assembler.Assemble("task", "step", "", "cap", "role")

	// Even if store returns a huge node, the store's RetrieveRelevantExperience
	// should have handled the budget, but here we test the assembler's logic.
	// Since our mock returns the node anyway, we check if the assembler handles it
	// (currently it relies on the store to respect the budget).

	if !strings.Contains(prompt, "## Current Task") {
		t.Errorf("Hot context should always be present")
	}
}
