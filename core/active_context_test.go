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

// TestActiveContextDoesNotRepeatUserTask verifies D-PROMPT-3 fix (§4.3):
// ActiveContext truncates the task spec to avoid duplicating the full text
// that already appears in the user message. A task spec > 200 chars should
// be truncated with a "see user message" hint.
func TestActiveContextDoesNotRepeatUserTask(t *testing.T) {
	assembler := NewActiveContextAssembler(nil, nil, 100)

	t.Run("short spec preserved", func(t *testing.T) {
		spec := "Analyze the attack surface"
		result := assembler.Assemble(context.Background(), spec, "step1", "", "cap", "role")
		if !strings.Contains(result, spec) {
			t.Error("short task spec should be preserved in ActiveContext")
		}
	})

	t.Run("long spec truncated", func(t *testing.T) {
		longSpec := strings.Repeat("x", 300)
		result := assembler.Assemble(context.Background(), longSpec, "step1", "", "cap", "role")
		if strings.Contains(result, longSpec) {
			t.Error("full 300-char task spec should NOT appear in ActiveContext (D-PROMPT-3)")
		}
		if !strings.Contains(result, "see user message") {
			t.Error("ActiveContext should hint to see user message for full spec")
		}
	})
}
