package store

import (
	"context"
	"testing"
)

type mockAPTEmbedder struct{}

func (m *mockAPTEmbedder) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	return []float32{0.1, 0.2, 0.3}, "mock", nil
}

type mockAPBackend struct{}

func (m *mockAPBackend) Upsert(ctx context.Context, p AntiPatternPrecedent) error    { return nil }
func (m *mockAPBackend) LoadAll(ctx context.Context) ([]AntiPatternPrecedent, error) { return nil, nil }

func TestAntiPatternStore_UpsertAddAndReplace(t *testing.T) {
	es := NewAntiPatternStore(nil, nil)
	ctx := context.Background()

	p1 := AntiPatternPrecedent{
		ID:               "prec_patch_large",
		AntiPattern:      "patch large files",
		CorrectPattern:   "read_file + write_file",
		Category:         "golang_development",
		TriggerCondition: "file_lines > 1000",
		Symptom:          "anchor drift",
		Confidence:       0.9,
		SourceText:       "patch fails on files over 1000 lines",
	}
	p2 := AntiPatternPrecedent{
		ID:               "prec_asset_threshold",
		AntiPattern:      "assume NewAssetManager(0) disables threshold",
		CorrectPattern:   "use threshold=1 to force side-load; default fallback is 50KB",
		Category:         "golang_development",
		TriggerCondition: "calling NewAssetManager with 0",
		Symptom:          "silent inline fallback",
		Confidence:       0.85,
	}

	es.Upsert(ctx, p1)
	es.Upsert(ctx, p2)

	if count := es.PrecedentCount(); count != 2 {
		t.Fatalf("expected 2 precedents, got %d", count)
	}

	// Upsert same ID: should replace, not duplicate.
	p1.Confidence = 0.99
	es.Upsert(ctx, p1)
	if count := es.PrecedentCount(); count != 2 {
		t.Fatalf("expected 2 after upsert replace, got %d", count)
	}

	top := es.TopConfidence(1)
	if len(top) != 1 || top[0].ID != "prec_patch_large" {
		t.Fatalf("expected top to be prec_patch_large, got %v", top)
	}
}

func TestAntiPatternStore_QueryByCategory(t *testing.T) {
	es := NewAntiPatternStore(nil, nil)
	ctx := context.Background()

	p1 := AntiPatternPrecedent{ID: "a", Category: "golang_development", AntiPattern: "mock", CorrectPattern: "do this", Confidence: 0.8}
	p2 := AntiPatternPrecedent{ID: "b", Category: "testing", AntiPattern: "mock", CorrectPattern: "do that", Confidence: 0.7}
	es.Upsert(ctx, p1)
	es.Upsert(ctx, p2)

	got := es.QueryByCategory("golang_development")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("category filter mismatch: got %v", got)
	}
}

func TestAntiPatternStore_QueryByKeyword(t *testing.T) {
	es := NewAntiPatternStore(nil, nil)
	ctx := context.Background()

	p1 := AntiPatternPrecedent{ID: "a", Symptom: "anchor drift", AntiPattern: "patch", CorrectPattern: "write_file", Confidence: 0.8}
	p2 := AntiPatternPrecedent{ID: "b", Symptom: "silent inline fallback", AntiPattern: "asset threshold", CorrectPattern: "threshold=1", Confidence: 0.7}
	es.Upsert(ctx, p1)
	es.Upsert(ctx, p2)

	got := es.QueryByKeyword("anchor", 5)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("keyword anchor: got %v", got)
	}
	got = es.QueryByKeyword("inline", 5)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("keyword inline: got %v", got)
	}
}

func TestAntiPatternStore_EmbeddingAutoInjection(t *testing.T) {
	es := NewAntiPatternStore(func() IEmbeddingClient { return &mockAPTEmbedder{} }, nil)
	ctx := context.Background()

	p := AntiPatternPrecedent{
		ID:             "prec_embedded",
		AntiPattern:    "test",
		CorrectPattern: "ok",
		SourceText:     "a source text that should be embedded",
		Confidence:     0.5,
	}
	es.Upsert(ctx, p)

	got := es.TopConfidence(1)
	if len(got) != 1 || len(got[0].Embedding) == 0 {
		t.Fatalf("expected embedding to be auto-populated")
	}
	if got[0].EmbeddingModel != "mock" {
		t.Fatalf("expected mock model, got %s", got[0].EmbeddingModel)
	}
}

func TestAntiPatternStore_TopConfidenceLimit(t *testing.T) {
	es := NewAntiPatternStore(nil, nil)
	ctx := context.Background()
	for i, conf := range []float64{0.1, 0.9, 0.5, 0.7, 0.3} {
		es.Upsert(ctx, AntiPatternPrecedent{ID: string(rune('a' + i)), Confidence: conf})
	}
	got := es.TopConfidence(2)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[0].Confidence != 0.9 || got[1].Confidence != 0.7 {
		t.Fatalf("expected descending confidence 0.9, 0.7, got %v", got)
	}
}

func TestAntiPatternStore_ContainsFold(t *testing.T) {
	es := NewAntiPatternStore(nil, nil)
	ctx := context.Background()
	es.Upsert(ctx, AntiPatternPrecedent{ID: "case", Symptom: "Anchor Drift Detected", CorrectPattern: "use WriteFile", Confidence: 0.6})
	got := es.QueryByKeyword("ANCHOR", 5)
	if len(got) != 1 || got[0].ID != "case" {
		t.Fatalf("case-insensitive keyword failed: got %v", got)
	}
}
