package core

import (
	"testing"
)

// TestClassifyBranchIntent_ContinueWhenSimilarToLeaf (Mode 3, 2026-10-24):
// a message similar to the last message → continue (no fork, no quote).
func TestClassifyBranchIntent_ContinueWhenSimilarToLeaf(t *testing.T) {
	s := &ChatSession{
		Messages:     map[string]*ChatMessage{},
		ActiveLeafID: "m2",
	}
	s.Messages["m1"] = &ChatMessage{ID: "m1", ParentID: "", Content: "tell me about Go programming"}
	s.Messages["m2"] = &ChatMessage{ID: "m2", ParentID: "m1", Content: "Go is a compiled language with goroutines"}

	d := ClassifyBranchIntent("what about goroutines in Go?", s)
	if d.Mode != "continue" {
		t.Fatalf("expected continue, got %s: %s", d.Mode, d.Reasoning)
	}
}

// TestClassifyBranchIntent_QuoteWhenSimilarToEarlier (Mode 3):
// a message similar to an EARLIER message (not the leaf) → quote (KV-cache-friendly).
func TestClassifyBranchIntent_QuoteWhenSimilarToEarlier(t *testing.T) {
	s := &ChatSession{
		Messages:     map[string]*ChatMessage{},
		ActiveLeafID: "m3",
	}
	s.Messages["m1"] = &ChatMessage{ID: "m1", ParentID: "", Content: "how do I configure the database connection pool"}
	s.Messages["m2"] = &ChatMessage{ID: "m2", ParentID: "m1", Content: "you can set max_connections in the config file"}
	s.Messages["m3"] = &ChatMessage{ID: "m3", ParentID: "m2", Content: "also check the timeout settings"}

	// This message is about the database config (m1), not the timeout (m3).
	d := ClassifyBranchIntent("regarding the database connection pool configuration", s)
	if d.Mode != "quote" {
		t.Fatalf("expected quote, got %s: %s", d.Mode, d.Reasoning)
	}
	if d.QuoteTargetID != "m1" {
		t.Fatalf("expected quote target m1, got %s", d.QuoteTargetID)
	}
}

// TestClassifyBranchIntent_ForkOnTopicShift (Mode 3):
// a message with low similarity to everything → fork (topic shift).
func TestClassifyBranchIntent_ForkOnTopicShift(t *testing.T) {
	s := &ChatSession{
		Messages:     map[string]*ChatMessage{},
		ActiveLeafID: "m2",
	}
	s.Messages["m1"] = &ChatMessage{ID: "m1", ParentID: "", Content: "let us discuss quantum physics"}
	s.Messages["m2"] = &ChatMessage{ID: "m2", ParentID: "m1", Content: "the Schrodinger equation describes wave functions"}

	d := ClassifyBranchIntent("completely unrelated topic about cooking pasta recipes", s)
	if d.Mode != "fork" {
		t.Fatalf("expected fork, got %s: %s", d.Mode, d.Reasoning)
	}
}

// TestKeywordSimilarity basic sanity.
func TestKeywordSimilarity(t *testing.T) {
	if s := keywordSimilarity("hello world", "world hello"); s != 1.0 {
		t.Fatalf("expected 1.0 for identical words, got %f", s)
	}
	if s := keywordSimilarity("hello world", "goodbye universe"); s != 0.0 {
		t.Fatalf("expected 0.0 for no overlap, got %f", s)
	}
	if s := keywordSimilarity("", "test"); s != 0.0 {
		t.Fatalf("expected 0.0 for empty, got %f", s)
	}
}
