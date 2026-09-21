package store

import (
	"context"
	"os"
	"testing"
)

func TestExperienceStore_QueryRelevantAntiPatterns(t *testing.T) {
	dir, err := os.MkdirTemp("", "ap_query_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// seedCanonicalAntiPatterns runs automatically during NewExperienceStore → loadSeeds
	results := es.QueryRelevantAntiPatterns("patch large files anchor drift", 5)
	if len(results) == 0 {
		t.Fatal("expected patch-anti-pattern to be returned")
	}
	foundPatch := false
	for _, r := range results {
		if r.ID == "prec_antipattern_large_file_patch" {
			foundPatch = true
		}
	}
	if !foundPatch {
		t.Fatalf("prec_antipattern_large_file_patch not in results: %v", results)
	}
}

func TestExperienceStore_QueryRelevantAntiPatterns_NoKeywordMatchReturnsNil(t *testing.T) {
	dir, err := os.MkdirTemp("", "ap_fallback_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// No keyword will match; should return nil (no cross-pollution fallback).
	// FIX (2026-09-15): TopConfidence fallback was removed to prevent
	// cross-task/cross-domain anti-pattern pollution.
	results := es.QueryRelevantAntiPatterns("xyznonexistentfoo", 3)
	if len(results) != 0 {
		t.Fatalf("expected nil results for no keyword match, got %d", len(results))
	}
}

func TestExperienceStore_QueryRelevantAntiPatterns_EmptyIntent(t *testing.T) {
	dir, err := os.MkdirTemp("", "ap_empty_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	results := es.QueryRelevantAntiPatterns("", 5)
	if len(results) != 0 {
		t.Fatalf("expected nil results for empty intent, got %d", len(results))
	}
}

func TestExperienceStore_CanonicalAntiPatternsSeededAfterLoad(t *testing.T) {
	dir, err := os.MkdirTemp("", "ap_seed_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	expected := map[string]bool{
		"prec_antipattern_large_file_patch":       true,
		"prec_antipattern_assetmanager_threshold": true,
		"prec_antipattern_mock_interface":         true,
		"prec_antipattern_static_threshold":       true,
		"prec_antipattern_role_protected_context": true,
	}
	if es.AntiPatternStore.PrecedentCount() < len(expected) {
		t.Fatalf("expected at least %d canonical precedents, got %d", len(expected), es.AntiPatternStore.PrecedentCount())
	}

	_ = context.Background()
}
