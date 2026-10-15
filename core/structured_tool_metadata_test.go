package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/store"
)

// ── Phase 1: ToolMetadata struct & ToolInteraction.Meta field ───────────────

func TestToolMetadata_FieldsAndJSON(t *testing.T) {
	meta := &store.ToolMetadata{
		Total:     100,
		Returned:  50,
		Truncated: true,
		Hint:      "fetch next page with offset=50",
		DataShape: "futures_dominant_daily",
	}
	if meta.Total != 100 || meta.Returned != 50 || !meta.Truncated {
		t.Error("ToolMetadata fields not set correctly")
	}
	if meta.Hint != "fetch next page with offset=50" {
		t.Error("Hint field not set correctly")
	}
	if meta.DataShape != "futures_dominant_daily" {
		t.Error("DataShape field not set correctly")
	}
}

func TestToolInteraction_WithMeta(t *testing.T) {
	ti := store.ToolInteraction{
		ToolName:  "get_data",
		Arguments: map[string]any{"symbol": "AAPL"},
		Result:    "some result",
		Meta: &store.ToolMetadata{
			Truncated: true,
			Total:     200,
			Returned:  50,
		},
	}
	if ti.Meta == nil || !ti.Meta.Truncated {
		t.Error("ToolInteraction.Meta not set correctly")
	}
}

func TestToolInteraction_NilMetaByDefault(t *testing.T) {
	ti := store.ToolInteraction{
		ToolName: "get_data",
		Result:   "result",
	}
	if ti.Meta != nil {
		t.Error("Meta should be nil by default")
	}
}

// ── Phase 2: extractMeta helper ─────────────────────────────────────────────

func TestExtractMeta_FromMapWithMeta(t *testing.T) {
	result := map[string]any{
		"data": "some data",
		"meta": map[string]any{
			"total":      float64(100),
			"returned":   float64(50),
			"truncated":  true,
			"hint":       "use offset=50",
			"data_shape": "daily_bars",
		},
	}
	meta := extractMeta(result)
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if meta.Total != 100 {
		t.Errorf("expected Total=100, got %d", meta.Total)
	}
	if meta.Returned != 50 {
		t.Errorf("expected Returned=50, got %d", meta.Returned)
	}
	if !meta.Truncated {
		t.Error("expected Truncated=true")
	}
	if meta.Hint != "use offset=50" {
		t.Errorf("expected Hint='use offset=50', got %q", meta.Hint)
	}
	if meta.DataShape != "daily_bars" {
		t.Errorf("expected DataShape='daily_bars', got %q", meta.DataShape)
	}
}

func TestExtractMeta_FromMapWithoutMeta(t *testing.T) {
	result := map[string]any{"data": "some data"}
	meta := extractMeta(result)
	if meta != nil {
		t.Error("expected nil meta when no 'meta' key present")
	}
}

func TestExtractMeta_FromNonMap(t *testing.T) {
	meta := extractMeta("just a string")
	if meta != nil {
		t.Error("expected nil meta for non-map result")
	}
	meta = extractMeta(42)
	if meta != nil {
		t.Error("expected nil meta for non-map result")
	}
}

func TestExtractMeta_MetaNotAMap(t *testing.T) {
	result := map[string]any{"meta": "not a map"}
	meta := extractMeta(result)
	if meta != nil {
		t.Error("expected nil meta when meta value is not a map")
	}
}

// ── Phase 3: InspectTruncationArtifact ──────────────────────────────────────

func TestSieve_InspectTruncationArtifact_NilMeta(t *testing.T) {
	s := NewSieve(100)
	ok, reason := s.InspectTruncationArtifact(nil, "all results")
	if !ok || reason != "" {
		t.Error("nil meta should pass")
	}
}

func TestSieve_InspectTruncationArtifact_NotTruncated(t *testing.T) {
	s := NewSieve(100)
	meta := &store.ToolMetadata{Truncated: false}
	ok, reason := s.InspectTruncationArtifact(meta, "all results")
	if !ok || reason != "" {
		t.Error("non-truncated meta should pass")
	}
}

func TestSieve_InspectTruncationArtifact_TruncatedWithUniversalQuantifier(t *testing.T) {
	s := NewSieve(100)
	meta := &store.ToolMetadata{
		Truncated: true,
		Returned:  50,
		Total:     100,
		Hint:      "fetch next page",
	}
	ok, reason := s.InspectTruncationArtifact(meta, "Here are all the results")
	if ok {
		t.Error("truncated + 'all' should be blocked")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestSieve_InspectTruncationArtifact_TruncatedWithChineseQuantifier(t *testing.T) {
	s := NewSieve(100)
	meta := &store.ToolMetadata{
		Truncated: true,
		Returned:  50,
		Total:     100,
	}
	ok, reason := s.InspectTruncationArtifact(meta, "这是全部数据")
	if ok {
		t.Error("truncated + '全部' should be blocked")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestSieve_InspectTruncationArtifact_TruncatedWithoutQuantifier(t *testing.T) {
	s := NewSieve(100)
	meta := &store.ToolMetadata{
		Truncated: true,
		Returned:  50,
		Total:     100,
	}
	ok, _ := s.InspectTruncationArtifact(meta, "Here are 50 of 100 results")
	if !ok {
		t.Error("truncated without universal quantifier should pass")
	}
}

func TestSieve_InspectTruncationArtifact_DefaultHint(t *testing.T) {
	s := NewSieve(100)
	meta := &store.ToolMetadata{
		Truncated: true,
		Returned:  50,
		Total:     100,
		// Hint intentionally empty
	}
	_, reason := s.InspectTruncationArtifact(meta, "all data")
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
	// Should contain the default hint
	if !strings.Contains(reason, "Do not claim comprehensive coverage") {
		t.Error("expected default hint in reason")
	}
}

// ── Phase 4: CheckDataShapeMismatch ─────────────────────────────────────────

func TestCheckDataShapeMismatch_NoInteractions(t *testing.T) {
	warning := CheckDataShapeMismatch(nil)
	if warning != "" {
		t.Error("expected empty warning for no interactions")
	}
}

func TestCheckDataShapeMismatch_SameShape(t *testing.T) {
	interactions := []store.ToolInteraction{
		{ToolName: "get_a", Meta: &store.ToolMetadata{DataShape: "daily"}},
		{ToolName: "get_b", Meta: &store.ToolMetadata{DataShape: "daily"}},
	}
	warning := CheckDataShapeMismatch(interactions)
	if warning != "" {
		t.Errorf("expected empty warning for same shape, got %q", warning)
	}
}

func TestCheckDataShapeMismatch_DifferentShapes(t *testing.T) {
	interactions := []store.ToolInteraction{
		{ToolName: "get_a", Meta: &store.ToolMetadata{DataShape: "daily_bars"}},
		{ToolName: "get_b", Meta: &store.ToolMetadata{DataShape: "hourly_bars"}},
	}
	warning := CheckDataShapeMismatch(interactions)
	if warning == "" {
		t.Error("expected warning for different shapes")
	}
}

func TestCheckDataShapeMismatch_NilMeta(t *testing.T) {
	interactions := []store.ToolInteraction{
		{ToolName: "get_a", Meta: nil},
		{ToolName: "get_b", Meta: nil},
	}
	warning := CheckDataShapeMismatch(interactions)
	if warning != "" {
		t.Error("expected empty warning when all Meta are nil")
	}
}

func TestCheckDataShapeMismatch_MixedNilAndNonNil(t *testing.T) {
	interactions := []store.ToolInteraction{
		{ToolName: "get_a", Meta: nil},
		{ToolName: "get_b", Meta: &store.ToolMetadata{DataShape: "daily"}},
		{ToolName: "get_c", Meta: &store.ToolMetadata{DataShape: "hourly"}},
	}
	warning := CheckDataShapeMismatch(interactions)
	if warning == "" {
		t.Error("expected warning when some Meta have different shapes")
	}
}
