package store

import (
	"context"
	"testing"
)

// ── RRF Fusion Unit Tests ──────────────────────────────────────────────────

func TestRRFFuse_EmptyInput(t *testing.T) {
	out := rrfFuse(nil)
	if len(out) != 0 {
		t.Fatalf("expected empty output, got %v", out)
	}
}

func TestRRFFuse_SingleElement(t *testing.T) {
	out := rrfFuse([]float64{5.0})
	if len(out) != 1 || out[0] != 0 {
		t.Fatalf("expected [0], got %v", out)
	}
}

func TestRRFFuse_PreservesOrderWhenScoresMatchCoarse(t *testing.T) {
	// Fine scores in descending order = same as coarse order (0,1,2,...).
	// RRF should preserve the original order.
	scores := []float64{10.0, 8.0, 6.0, 4.0}
	out := rrfFuse(scores)

	expected := []int{0, 1, 2, 3}
	for i := range out {
		if out[i] != expected[i] {
			t.Fatalf("position %d: expected %d, got %d. full: %v", i, expected[i], out[i], out)
		}
	}
}

func TestRRFFuse_ReordersWhenScoresDisagree(t *testing.T) {
	// Coarse order: [0, 1, 2, 3] (by slice position)
	// Fine scores:   [1, 9, 5, 7] → fine order by score desc: [1, 3, 2, 0]
	//
	// RRF scores (k=60):
	//   doc 0: 1/(60+0) + 1/(60+3) = 0.01667 + 0.01587 = 0.03254
	//   doc 1: 1/(60+1) + 1/(60+0) = 0.01639 + 0.01667 = 0.03306  ← highest
	//   doc 2: 1/(60+2) + 1/(60+2) = 0.01613 + 0.01613 = 0.03226
	//   doc 3: 1/(60+3) + 1/(60+1) = 0.01587 + 0.01639 = 0.03226
	//
	// Expected fused order: [1, 0, 2, 3] or [1, 0, 3, 2]
	// (docs 2 and 3 have equal RRF scores, so their relative order is stable)
	scores := []float64{1.0, 9.0, 5.0, 7.0}
	out := rrfFuse(scores)

	if len(out) != 4 {
		t.Fatalf("expected 4 elements, got %d", len(out))
	}
	// doc 1 should be first (highest RRF score)
	if out[0] != 1 {
		t.Fatalf("expected doc 1 first, got doc %d. full: %v", out[0], out)
	}
	// doc 0 should be second (second highest RRF score)
	if out[1] != 0 {
		t.Fatalf("expected doc 0 second, got doc %d. full: %v", out[1], out)
	}
}

func TestRRFFuse_FineRankDominates(t *testing.T) {
	// Extreme case: coarse rank strongly disagrees with fine rank.
	// Coarse order: [0, 1, 2] (doc 0 is "best" in coarse)
	// Fine scores:   [0, 0, 10] (doc 2 is "best" in fine)
	//
	// RRF scores (k=60):
	//   doc 0: 1/(60+0) + 1/(60+2) = 0.01667 + 0.01613 = 0.03280
	//   doc 1: 1/(60+1) + 1/(60+1) = 0.01639 + 0.01639 = 0.03279
	//   doc 2: 1/(60+2) + 1/(60+0) = 0.01613 + 0.01667 = 0.03280
	//
	// doc 0 and doc 2 tie (both 0.03280), doc 1 is slightly lower.
	// With k=60, coarse and fine contribute roughly equally.
	// The key property: RRF doesn't let either ranking dominate.
	scores := []float64{0.0, 0.0, 10.0}
	out := rrfFuse(scores)

	if len(out) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(out))
	}
	// All three docs should be present (permutation of [0,1,2])
	seen := make(map[int]bool)
	for _, idx := range out {
		if idx < 0 || idx > 2 || seen[idx] {
			t.Fatalf("invalid or duplicate index %d in output %v", idx, out)
		}
		seen[idx] = true
	}
}

func TestRRFFuse_AllEqualScores(t *testing.T) {
	// When all fine scores are equal, fine rank is arbitrary (stable sort
	// preserves original order). RRF should still produce a valid permutation.
	scores := []float64{5.0, 5.0, 5.0, 5.0}
	out := rrfFuse(scores)

	if len(out) != 4 {
		t.Fatalf("expected 4 elements, got %d", len(out))
	}
	seen := make(map[int]bool)
	for _, idx := range out {
		if idx < 0 || idx > 3 || seen[idx] {
			t.Fatalf("invalid or duplicate index %d in output %v", idx, out)
		}
		seen[idx] = true
	}
}

// ── Mock Reranker ──────────────────────────────────────────────────────────

type mockReranker struct {
	scores     []float64
	err        error
	called     bool
	querySeen  string
	docsSeen   []string
	callCount  int
}

func (m *mockReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	m.called = true
	m.callCount++
	m.querySeen = query
	m.docsSeen = documents
	if m.err != nil {
		return nil, m.err
	}
	return m.scores, nil
}

// ── RetrieveRelevantExperience + Reranker Integration Tests ────────────────

func TestRetrieveRelevantExperience_NoReranker_CoarseOrderPreserved(t *testing.T) {
	es := &ExperienceStore{
		Nodes:          make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	// Populate nodes with embeddings for Tier 2 retrieval.
	query := []float32{1.0, 0.0, 0.0}
	es.Nodes["a"] = &ExperienceNode{
		NodeID:     "a",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{1.0, 0.0, 0.0}, // perfect match
		SourceText: "node A",
	}
	es.Nodes["b"] = &ExperienceNode{
		NodeID:     "b",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{0.9, 0.1, 0.0}, // close match
		SourceText: "node B",
	}
	es.rebuildNodeIndexLocked()

	results := es.RetrieveRelevantExperience(context.Background(), query, "coding", "", 1000)
	if len(results) == 0 {
		t.Fatal("expected non-empty results")
	}
	// Without a reranker, results should be in coarse order (Tier 2 by similarity).
	// Node "a" has perfect similarity (1.0), node "b" has 0.9.
	if results[0].NodeID != "a" {
		t.Fatalf("expected node 'a' first (highest similarity), got '%s'", results[0].NodeID)
	}
}

func TestRetrieveRelevantExperience_WithReranker_AppliesRRF(t *testing.T) {
	es := &ExperienceStore{
		Nodes:            make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	// Populate 3 nodes with embeddings for Tier 2 retrieval.
	// Coarse order by cosine similarity: [a (1.0), b (0.95), c (0.9)]
	query := []float32{1.0, 0.0, 0.0}
	es.Nodes["a"] = &ExperienceNode{
		NodeID:     "a",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{1.0, 0.0, 0.0},
		SourceText: "node A",
	}
	es.Nodes["b"] = &ExperienceNode{
		NodeID:     "b",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{0.95, 0.05, 0.0},
		SourceText: "node B",
	}
	es.Nodes["c"] = &ExperienceNode{
		NodeID:     "c",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{0.9, 0.1, 0.0},
		SourceText: "node C",
	}
	es.rebuildNodeIndexLocked()

	// Reranker scores: [3, 9, 5] → fine order: [b, c, a]
	// Fine ranks: a→2, b→0, c→1
	//
	// RRF scores (k=60):
	//   a: 1/(60+0) + 1/(60+2) = 0.01667 + 0.01613 = 0.03280
	//   b: 1/(60+1) + 1/(60+0) = 0.01639 + 0.01667 = 0.03306  ← highest
	//   c: 1/(60+2) + 1/(60+1) = 0.01613 + 0.01639 = 0.03252
	//
	// Fused order: [b, a, c] — b is promoted from coarse rank 1 to fused rank 0.
	reranker := &mockReranker{scores: []float64{3.0, 9.0, 5.0}}
	es.SetRerankerClient(reranker)

	results := es.RetrieveRelevantExperience(context.Background(), query, "coding", "", 1000)

	if !reranker.called {
		t.Fatal("expected reranker to be called")
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// RRF should promote b to first place.
	if results[0].NodeID != "b" {
		t.Fatalf("expected node 'b' first after RRF reranking, got '%s'. order: %v",
			results[0].NodeID, []string{results[0].NodeID, results[1].NodeID, results[2].NodeID})
	}
}

func TestRetrieveRelevantExperience_RerankerFailure_GracefulDegradation(t *testing.T) {
	es := &ExperienceStore{
		Nodes:            make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	query := []float32{1.0, 0.0, 0.0}
	es.Nodes["a"] = &ExperienceNode{
		NodeID:     "a",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{1.0, 0.0, 0.0},
		SourceText: "node A",
	}
	es.Nodes["b"] = &ExperienceNode{
		NodeID:     "b",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{0.9, 0.1, 0.0},
		SourceText: "node B",
	}
	es.rebuildNodeIndexLocked()

	// Reranker returns an error — should fall back to coarse order.
	reranker := &mockReranker{err: context.DeadlineExceeded}
	es.SetRerankerClient(reranker)

	results := es.RetrieveRelevantExperience(context.Background(), query, "coding", "", 1000)

	if !reranker.called {
		t.Fatal("expected reranker to be called even on error")
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Should preserve coarse order (a first, since it has higher similarity).
	if results[0].NodeID != "a" {
		t.Fatalf("expected coarse order preserved on reranker failure, got '%s' first", results[0].NodeID)
	}
}

func TestRetrieveRelevantExperience_SingleResult_NoReranking(t *testing.T) {
	es := &ExperienceStore{
		Nodes:            make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	// Only one node — reranking a single element is pointless.
	es.Nodes["a"] = &ExperienceNode{
		NodeID:     "a",
		Capability: "coding",
		Outcome:    "failure",
		FailureMode: "timeout_exceeded",
		Embedding:  []float32{1.0, 0.0, 0.0},
		SourceText: "node A",
	}
	es.rebuildNodeIndexLocked()

	reranker := &mockReranker{scores: []float64{5.0}}
	es.SetRerankerClient(reranker)

	results := es.RetrieveRelevantExperience(
		context.Background(),
		nil, // no embedding query → skip Tier 2
		"coding",
		"timeout", // triggers Tier 1
		1000,
	)

	if reranker.called {
		t.Fatal("reranker should not be called for single result")
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestRetrieveRelevantExperience_RerankerScoreCountMismatch_GracefulDegradation(t *testing.T) {
	es := &ExperienceStore{
		Nodes:            make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	query := []float32{1.0, 0.0, 0.0}
	es.Nodes["a"] = &ExperienceNode{
		NodeID:     "a",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{1.0, 0.0, 0.0},
		SourceText: "node A",
	}
	es.Nodes["b"] = &ExperienceNode{
		NodeID:     "b",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{0.9, 0.1, 0.0},
		SourceText: "node B",
	}
	es.rebuildNodeIndexLocked()

	// Reranker returns wrong number of scores — should fall back to coarse order.
	reranker := &mockReranker{scores: []float64{5.0}} // only 1 score for 2 docs
	es.SetRerankerClient(reranker)

	results := es.RetrieveRelevantExperience(context.Background(), query, "coding", "", 1000)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Should preserve coarse order.
	if results[0].NodeID != "a" {
		t.Fatalf("expected coarse order preserved on score mismatch, got '%s' first", results[0].NodeID)
	}
}

func TestRetrieveRelevantExperience_RerankerQueryText(t *testing.T) {
	es := &ExperienceStore{
		Nodes:            make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	query := []float32{1.0, 0.0, 0.0}
	es.Nodes["a"] = &ExperienceNode{
		NodeID:     "a",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{1.0, 0.0, 0.0},
		SourceText: "node A",
	}
	es.Nodes["b"] = &ExperienceNode{
		NodeID:     "b",
		Capability: "coding",
		Outcome:    "failure",
		Embedding:  []float32{0.9, 0.1, 0.0},
		SourceText: "node B",
	}
	es.rebuildNodeIndexLocked()

	reranker := &mockReranker{scores: []float64{5.0, 5.0}}
	es.SetRerankerClient(reranker)

	es.RetrieveRelevantExperience(context.Background(), query, "coding", "rate_limit_hit", 1000)

	// Reranker should receive the errorSignal as query text.
	if reranker.querySeen != "rate_limit_hit" {
		t.Fatalf("expected query 'rate_limit_hit', got '%s'", reranker.querySeen)
	}
	// Reranker should receive document texts.
	if len(reranker.docsSeen) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(reranker.docsSeen))
	}
}

// ── RebuildNodeIndex Tests ─────────────────────────────────────────────────

func TestRebuildNodeIndex_AfterDirectMutation(t *testing.T) {
	es := &ExperienceStore{
		Nodes:            make(map[string]*ExperienceNode),
		AntiPatternStore: NewAntiPatternStore(nil, nil),
	}

	// Directly mutate Nodes (bypassing RecordTaskCompletion).
	es.Nodes["n1"] = &ExperienceNode{
		NodeID:      "n1",
		Capability:  "coding",
		Outcome:     "failure",
		FailureMode: "timeout_exceeded",
	}
	es.Nodes["n2"] = &ExperienceNode{
		NodeID:      "n2",
		Capability:  "coding",
		Outcome:     "failure",
		FailureMode: "rate_limit_hit",
	}
	// Success nodes should NOT be indexed (Tier 1 only matches failures).
	es.Nodes["n3"] = &ExperienceNode{
		NodeID:     "n3",
		Capability: "coding",
		Outcome:    "success",
	}

	// Before rebuild, index is nil — Tier 1 lookup returns nothing.
	results := es.RetrieveRelevantExperience(
		context.Background(), nil, "coding", "timeout", 1000,
	)
	if len(results) != 0 {
		t.Fatalf("expected 0 results before rebuild, got %d", len(results))
	}

	// Rebuild and verify Tier 1 now finds the seeded node.
	es.RebuildNodeIndex()
	results = es.RetrieveRelevantExperience(
		context.Background(), nil, "coding", "timeout", 1000,
	)
	if len(results) == 0 {
		t.Fatal("expected results after RebuildNodeIndex, got 0")
	}
	if results[0].NodeID != "n1" {
		t.Fatalf("expected node n1, got %s", results[0].NodeID)
	}
}
