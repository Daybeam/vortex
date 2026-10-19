// R4 regression tests — dedup loop termination bug + cosine dedup killing
// paired tools + non-deterministic sort.
//
// Bug A (tool_router.go, old line 360): the selection loop used
//
//	for i := 0; i < limit && !redundant[sorted[i].id]; i++
//
// which EXITS when it encounters a redundant tool, dropping all subsequent
// tools. If sorted[1] was marked redundant, sorted[2..4] were never selected.
//
// Bug B (tool_router.go, old line 383): cosine dedup (>0.85) marked paired
// tools like show_sent_payment_requests / show_received_payment_requests as
// redundant because their embeddings were near-identical, causing one to be
// silently dropped.
//
// Both bugs are fixed by removing the embedding dedup entirely. The sort is
// also made deterministic with a secondary key (id ascending) so that equal
// scores produce stable output order.

package core

import (
	"context"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestRoute_R4_PairedToolsNotDroppedByDedup verifies that tools with
// identical embeddings are not dropped from the routed output.
//
// Before the fix: the cosine dedup (>0.85) would mark the second paired
// tool as redundant. Bug A would then terminate the loop, dropping all
// subsequent tools — only 1 tool would be selected instead of 5.
//
// After the fix: the dedup is removed, so both paired tools survive and
// all slots up to limit=5 are filled.
func TestRoute_R4_PairedToolsNotDroppedByDedup(t *testing.T) {
	tools := []string{
		"venmo__show_sent_payment_requests",
		"venmo__show_received_payment_requests",
		"venmo__send_payment",
		"venmo__get_balance",
		"venmo__show_profile",
		"venmo__add_note",
		"venmo__get_user",
	}

	docs := map[string]string{
		"venmo__show_sent_payment_requests":     "show sent payment requests history",
		"venmo__show_received_payment_requests": "show received payment requests history",
		"venmo__send_payment":                   "send payment to another user",
		"venmo__get_balance":                    "get current account balance",
		"venmo__show_profile":                   "show user profile information",
		"venmo__add_note":                       "add note to existing payment",
		"venmo__get_user":                       "get user details by id",
	}

	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: tools,
			},
		},
	}

	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)

	// Identical embeddings for the paired tools — before the fix, cosine
	// dedup (>0.85) would mark one as redundant and the loop would terminate.
	pairedVec := []float32{1.0, 0.5, 0.3}
	router.embeddings = map[string][]float32{
		"venmo__show_sent_payment_requests":     pairedVec,
		"venmo__show_received_payment_requests": pairedVec, // identical → cosine = 1.0
		"venmo__send_payment":                   {0.8, 0.2, 0.1},
		"venmo__get_balance":                    {0.1, 0.9, 0.0},
		"venmo__show_profile":                   {0.3, 0.3, 0.9},
		"venmo__add_note":                       {0.0, 0.1, 0.8},
		"venmo__get_user":                       {0.2, 0.0, 0.7},
	}

	bindings := []config.MCPBinding{
		{MCPID: "mcp1"},
	}

	req := RouteRequest{
		Task:     "show sent and received payment requests",
		Bindings: bindings,
	}

	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}

	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}

	// Bug B regression: both paired tools must survive.
	if !allowed["venmo__show_sent_payment_requests"] {
		t.Error("venmo__show_sent_payment_requests was dropped — R4 Bug B: cosine dedup killed paired tool")
	}
	if !allowed["venmo__show_received_payment_requests"] {
		t.Error("venmo__show_received_payment_requests was dropped — R4 Bug B: cosine dedup killed paired tool")
	}

	// Bug A regression: all slots up to limit=5 should be filled.
	// Before the fix, the loop would terminate at the first redundant tool.
	if len(routed[0].AllowedTools) < 5 {
		t.Errorf("expected at least 5 routed tools, got %d — R4 Bug A: loop terminated early on redundant tool",
			len(routed[0].AllowedTools))
	}
}

// TestRoute_R4_DeterministicSortEqualScores verifies that tools with equal
// merged scores are ordered deterministically by id (secondary sort key).
//
// Before the fix: sort.Slice used only score as the comparator, producing
// non-deterministic order for equal scores (Go's sort.Slice is not stable).
// This caused flaky tool selection across runs, invalidating prompt-cache
// prefixes.
//
// After the fix: the secondary key (id ascending) guarantees stable output.
func TestRoute_R4_DeterministicSortEqualScores(t *testing.T) {
	// All tools share the same description → identical BM25 scores.
	// Tool names are deliberately out of alphabetical order to catch
	// non-deterministic sorting.
	tools := []string{
		"app__delta",
		"app__alpha",
		"app__epsilon",
		"app__beta",
		"app__gamma",
	}

	docs := make(map[string]string)
	for _, tool := range tools {
		docs[tool] = "common test tool"
	}

	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: tools,
			},
		},
	}

	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)

	bindings := []config.MCPBinding{
		{MCPID: "mcp1"},
	}

	req := RouteRequest{
		Task:     "common test tool",
		Bindings: bindings,
	}

	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}

	got := routed[0].AllowedTools
	// All 5 tools should be selected (limit=5, all have equal scores).
	if len(got) != 5 {
		t.Fatalf("expected 5 routed tools, got %d: %v", len(got), got)
	}

	// Expected order: alphabetical by id (secondary sort key).
	expected := []string{
		"app__alpha",
		"app__beta",
		"app__delta",
		"app__epsilon",
		"app__gamma",
	}

	for i, want := range expected {
		if got[i] != want {
			t.Errorf("position %d: expected %q, got %q — R4: non-deterministic sort for equal scores",
				i, want, got[i])
		}
	}
}
