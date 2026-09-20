package core

import (
	"math"
	"testing"
)

func TestBM25Corpus(t *testing.T) {
	docs := map[string]string{
		"doc1": "the quick brown fox",
		"doc2": "the lazy dog jumps over the fox",
		"doc3": "red fox jumps",
	}
	corpus := NewBM25Corpus(1.2, 0.75, docs)

	t.Run("Term Presence", func(t *testing.T) {
		scores := corpus.Score("fox")
		if len(scores) != 3 {
			t.Errorf("expected 3 results for 'fox', got %d", len(scores))
		}
		if scores["doc1"] <= 0 || scores["doc2"] <= 0 || scores["doc3"] <= 0 {
			t.Errorf("expected positive scores for 'fox'")
		}
	})

	t.Run("Ranking Order", func(t *testing.T) {
		scores := corpus.Score("quick brown")
		if scores["doc1"] <= scores["doc2"] || scores["doc1"] <= scores["doc3"] {
			t.Errorf("doc1 should rank highest for 'quick brown'")
		}
	})
}

func TestRRFMerge(t *testing.T) {
	rank1 := map[string]float64{"a": 1.0, "b": 0.8, "c": 0.5}
	rank2 := map[string]float64{"b": 1.0, "a": 0.2, "d": 0.1}

	merged := RRFMerge(rank1, rank2)

	// a: rank 0 (rank1), rank 1 (rank2) -> 1/(60+0) + 1/(60+1) = 0.016666 + 0.016393 = 0.033059
	// b: rank 1 (rank1), rank 0 (rank2) -> 1/(60+1) + 1/(60+0) = 0.033059
	// c: rank 2 (rank1) -> 1/(60+2) = 0.016129
	// d: rank 2 (rank2) -> 1/(60+2) = 0.016129

	if math.Abs(merged["a"]-merged["b"]) > 0.000001 {
		t.Errorf("expected a and b to have same RRF score")
	}
	if merged["a"] <= merged["c"] {
		t.Errorf("expected a to rank higher than c")
	}
}

func TestTokenize(t *testing.T) {
	tokens := tokenize("The Quick Brown Fox! 123")
	if tokens["quick"] != 1 || tokens["brown"] != 1 || tokens["fox"] != 1 || tokens["123"] != 1 {
		t.Errorf("unexpected tokenization: %v", tokens)
	}
}
