// R1 regression test — BM25 corpus leakage.
//
// Bug: corpus.Score(req.Task) scores the ENTIRE corpus, not just the
// available tools for the current binding. After RRF merge, a tool that
// is in the corpus but NOT in the binding's allowlist can appear in the
// routed output — a security violation.
//
// Fix: filter bm25Rank to the available set before merging.

package core

import (
	"context"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestRoute_R1_BM25DoesNotLeakOutsideAvailable verifies that a tool which
// scores highly in BM25 but is NOT in the binding's allowlist never
// appears in the routed output.
//
// Before the fix: BM25 scored the entire corpus. A tool outside the
// binding's allowlist could get a high BM25 score, survive RRF merge
// (as the sole contributor), and be selected into the output.
//
// After the fix: bm25Rank is filtered to the available set before merge,
// so unauthorized tools can never enter the selection.
func TestRoute_R1_BM25DoesNotLeakOutsideAvailable(t *testing.T) {
	// The corpus contains 5 tools. The binding only allows 3 of them.
	// The task text is crafted to strongly match the unauthorized tool.
	allTools := []string{
		"tool_a",
		"tool_b",
		"tool_c",
		"tool_d", // in corpus, NOT in binding's allowlist
		"tool_e",
	}

	docs := map[string]string{
		"tool_a": "alpha general utility",
		"tool_b": "beta general utility",
		"tool_c": "gamma general utility",
		"tool_d": "delta special unique keyword match",
		"tool_e": "epsilon general utility",
	}

	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: allTools,
			},
		},
	}

	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)

	// Restricted binding: only tool_a, tool_b, tool_c are authorized.
	// tool_d is in the corpus and will score highly, but is NOT authorized.
	bindings := []config.MCPBinding{
		{
			MCPID:        "mcp1",
			AllowedTools: []string{"tool_a", "tool_b", "tool_c"},
		},
	}

	req := RouteRequest{
		Task:     "delta special unique keyword match",
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

	// R1 regression: tool_d must NOT appear — it was never authorized.
	if allowed["tool_d"] {
		t.Error("tool_d leaked into routed output — R1: BM25 scored entire corpus, " +
			"unauthorized tool entered RRF merge")
	}

	// Every routed tool must be in the original allowlist.
	authorized := map[string]bool{"tool_a": true, "tool_b": true, "tool_c": true}
	for tool := range allowed {
		if !authorized[tool] {
			t.Errorf("routed tool %q is not in the authorized set — R1: output ⊄ authorized", tool)
		}
	}
}

// TestRoute_R1_BM25DoesNotLeakUnrestricted verifies the same property
// for unrestricted bindings: a tool in the corpus but NOT in the MCP's
// AvailableTools must not appear in the output.
func TestRoute_R1_BM25DoesNotLeakUnrestricted(t *testing.T) {
	docs := map[string]string{
		"authorized_1": "first authorized tool",
		"authorized_2": "second authorized tool",
		"stale_tool":   "stale tool still in corpus but removed from MCP",
	}

	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				// AvailableTools does NOT include stale_tool.
				AvailableTools: []string{"authorized_1", "authorized_2"},
			},
		},
	}

	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)

	// Unrestricted binding (empty AllowedTools = all of AvailableTools).
	bindings := []config.MCPBinding{
		{MCPID: "mcp1"},
	}

	req := RouteRequest{
		Task:     "stale tool still in corpus but removed from MCP",
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

	if allowed["stale_tool"] {
		t.Error("stale_tool leaked into routed output — R1: BM25 scored entire corpus, " +
			"tool not in AvailableTools entered RRF merge")
	}
}
