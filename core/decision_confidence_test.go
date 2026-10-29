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

// TestQueryDecisionPrecedents_ConfidenceWeightedRanking verifies that when two
// precedents have similar semantic relevance (same embedding), the one with
// higher Confidence ranks first. This is the core behaviour of memory tiering
// gap B (design §1.4 方案 B, 2026-10-23).
func TestQueryDecisionPrecedents_ConfidenceWeightedRanking(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es := mustNewExperienceStore(t, tmpDir, ts, &config.SystemSettings{}, nil, nil)

	client := &precedentMockEmbeddingClient{}
	es.SetEmbeddingClient(client)

	// Two precedents with identical reasoning → identical embeddings → identical
	// cosine similarity. The only differentiator is Confidence.
	highConf := &schemas.DecisionNode{
		ID:        "dec-high-conf",
		Reasoning: "test intent", // same text → same embedding as dec-low-conf
		Action:    "verified_action",
		Outcome:   "success",
		Source:    "verifier",
		Confidence: 0.95,
		Timestamp:  time.Now(),
	}
	lowConf := &schemas.DecisionNode{
		ID:        "dec-low-conf",
		Reasoning: "test intent", // same text → same embedding
		Action:    "guessed_action",
		Outcome:   "partial",
		Source:    "model",
		Confidence: 0.2,
		Timestamp:  time.Now(),
	}

	if err := es.UpsertDecisionPrecedent(context.Background(), highConf); err != nil {
		t.Fatalf("Upsert highConf: %v", err)
	}
	if err := es.UpsertDecisionPrecedent(context.Background(), lowConf); err != nil {
		t.Fatalf("Upsert lowConf: %v", err)
	}

	// Wait for async embedding goroutines
	time.Sleep(200 * time.Millisecond)

	results := es.QueryDecisionPrecedents(context.Background(), "test intent", 5)
	if len(results) != 2 {
		t.Fatalf("expected 2 precedents, got %d", len(results))
	}

	if results[0].ID != "dec-high-conf" {
		t.Errorf("expected dec-high-conf first (confidence-weighted ranking), got %s",
			results[0].ID)
	}
	if results[1].ID != "dec-low-conf" {
		t.Errorf("expected dec-low-conf second, got %s", results[1].ID)
	}
}

// TestQueryDecisionPrecedents_ConfidenceWeightedCrossRanking verifies that a
// precedent with slightly lower cosine similarity but much higher confidence
// can outrank one with higher similarity but very low confidence.
func TestQueryDecisionPrecedents_ConfidenceWeightedCrossRanking(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es := mustNewExperienceStore(t, tmpDir, ts, &config.SystemSettings{}, nil, nil)

	client := &precedentMockEmbeddingClient{}
	es.SetEmbeddingClient(client)

	// "test intent" → embedding [1,0,0,0]; query "similar intent" → [0.9,0.1,0,0]
	// cosine ≈ 0.994. With conf=0.95 → effective ≈ 0.994 * 0.975 ≈ 0.969.
	highSimHighConf := &schemas.DecisionNode{
		ID:         "dec-sim-hc",
		Reasoning:  "test intent",
		Action:     "a1",
		Outcome:    "ok",
		Confidence: 0.95,
		Timestamp:  time.Now(),
	}

	// "similar intent" → embedding [0.9,0.1,0,0]; query "similar intent" → [0.9,0.1,0,0]
	// cosine = 1.0. With conf=0.0 → effective = 1.0 * 0.5 = 0.5.
	// So highSimHighConf (0.969) should outrank this (0.5) despite lower cosine.
	highSimZeroConf := &schemas.DecisionNode{
		ID:        "dec-sim-zc",
		Reasoning: "similar intent",
		Action:    "a2",
		Outcome:   "ok",
		Timestamp: time.Now(), // Confidence = 0 (zero value)
	}

	if err := es.UpsertDecisionPrecedent(context.Background(), highSimHighConf); err != nil {
		t.Fatalf("Upsert highSimHighConf: %v", err)
	}
	if err := es.UpsertDecisionPrecedent(context.Background(), highSimZeroConf); err != nil {
		t.Fatalf("Upsert highSimZeroConf: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	results := es.QueryDecisionPrecedents(context.Background(), "similar intent", 5)
	if len(results) != 2 {
		t.Fatalf("expected 2 precedents, got %d", len(results))
	}

	// highSimHighConf has cosine≈0.994 * confWeight 0.975 ≈ 0.969
	// highSimZeroConf has cosine=1.0 * confWeight 0.5 = 0.5
	// So highSimHighConf should rank first despite slightly lower cosine.
	if results[0].ID != "dec-sim-hc" {
		t.Errorf("expected dec-sim-hc first (confidence outranks pure cosine), got %s",
			results[0].ID)
	}
}

// TestSummarizePrecedent_ConfidenceAnnotation verifies that SummarizePrecedent
// annotates the confidence level when Confidence > 0.
func TestSummarizePrecedent_ConfidenceAnnotation(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)

	cases := []struct {
		name      string
		conf      float64
		wantTag   string
		wantAnnot bool
	}{
		{"high_conf_conclusive", 0.9, "conclusive", true},
		{"threshold_conf_conclusive", 0.7, "conclusive", true},
		{"low_conf_advisory", 0.3, "advisory", true},
		{"very_low_conf_advisory", 0.01, "advisory", true},
		{"zero_conf_no_annotation", 0.0, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &schemas.DecisionNode{
				Outcome:    "test outcome",
				Reasoning:  "test reasoning",
				Action:     "test action",
				Confidence: tc.conf,
			}
			got := e.SummarizePrecedent(p)

			if !tc.wantAnnot {
				if strings.Contains(got, "Confidence") {
					t.Errorf("expected no confidence annotation for conf=0, got: %s", got)
				}
				return
			}

			if !strings.Contains(got, "Confidence") {
				t.Errorf("expected confidence annotation, got: %s", got)
			}
			if !strings.Contains(got, tc.wantTag) {
				t.Errorf("expected tag %q, got: %s", tc.wantTag, got)
			}
		})
	}
}

// TestSummarizePrecedent_BackwardCompat verifies that a DecisionNode without
// Confidence (zero value) produces the same output as before the annotation
// was added.
func TestSummarizePrecedent_BackwardCompat(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	p := &schemas.DecisionNode{
		Outcome:   "refactored auth module",
		Reasoning: "The auth module had circular imports. I broke them by extracting an interface.",
		Action:    "extracted AuthService interface",
	}
	got := e.SummarizePrecedent(p)

	// Must NOT contain the confidence annotation line
	if strings.Contains(got, "Confidence") {
		t.Errorf("backward compat broken: confidence annotation added for conf=0, got: %s", got)
	}
	// Must still contain the core content
	if !strings.Contains(got, "refactored auth module") {
		t.Errorf("missing Outcome, got: %s", got)
	}
	if !strings.Contains(got, "extracted AuthService interface") {
		t.Errorf("missing Action, got: %s", got)
	}
}
