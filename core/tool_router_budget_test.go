// Regression tests for the per-step total budget and the break→continue fix.
//
// Bug A (per-binding budget): the token budget was applied per-binding, so
// N MCPs each got their own 8000-token ceiling. The real ceiling was N*8000,
// defeating the purpose of context economy.
//
// Bug B (break vs continue): when a tool exceeded the remaining budget, the
// selection loop broke — abandoning all subsequent tools. A later, smaller
// tool that would have fit was never tried.

package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// makeDesc returns a description string whose length makes estimateToolTokens
// return approximately targetTokens for a tool with the given name and no
// input schema. The formula is: tokens = (len(name)+len(desc))/4 + 8.
func makeDesc(name string, targetTokens int) string {
	descLen := (targetTokens - 8) * 4
	if descLen < 0 {
		descLen = 0
	}
	descLen -= len(name)
	if descLen < 0 {
		descLen = 0
	}
	return strings.Repeat("x", descLen)
}

// toolDef creates a ToolDefinition with a description sized so that
// estimateToolTokens returns approximately targetTokens.
func toolDef(name string, targetTokens int) schemas.ToolDefinition {
	return schemas.ToolDefinition{
		Name:        name,
		Description: makeDesc(name, targetTokens),
	}
}

// TestRoute_BudgetContinueNotBreak verifies that an oversized tool in the
// middle of the ranking does not prevent smaller tools after it from being
// selected.
//
// Before the fix: the loop broke at the oversized tool, so the small tool
// after it was never tried.
//
// After the fix: the loop continues past the oversized tool, and the small
// tool is admitted if it fits the remaining budget.
func TestRoute_BudgetContinueNotBreak(t *testing.T) {
	// Three tools: big (500 tokens), medium (300 tokens), small (50 tokens).
	// Task matches "big" most strongly, then "medium", then "small".
	// Budget: 600 tokens.
	//
	// With break: big is selected (500), medium exceeds (500+300 > 600),
	//   loop terminates, small never tried. Result: [big] — 1 tool.
	// With continue: big is selected (500), medium skipped, small fits
	//   (500+50 ≤ 600), selected. Result: [big, small] — 2 tools.
	tools := []string{"big_tool", "medium_tool", "small_tool"}
	docs := map[string]string{
		"big_tool":    "big relevant tool",
		"medium_tool": "medium relevant tool",
		"small_tool":  "small relevant tool",
	}

	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: tools,
				FullToolDefinitions: []schemas.ToolDefinition{
					toolDef("big_tool", 500),
					toolDef("medium_tool", 300),
					toolDef("small_tool", 50),
				},
			},
		},
	}

	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)

	routed := router.Route(context.Background(), RouteRequest{
		Task:            "big relevant tool medium relevant tool small relevant tool",
		Bindings:        []config.MCPBinding{{MCPID: "mcp1"}},
		ProgressiveMode: false,
		MaxTokenBudget:  600,
	})
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}

	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}

	// big_tool must always be selected (first entry is always admitted).
	if !allowed["big_tool"] {
		t.Error("big_tool must be selected — first entry is always admitted")
	}

	// small_tool must be selected — this is the break→continue regression.
	// With break, the loop terminated at medium_tool and small_tool was
	// never tried.
	if !allowed["small_tool"] {
		t.Error("small_tool was not selected — budget loop broke at medium_tool " +
			"instead of continuing to small_tool")
	}
}

// TestRoute_BudgetPerStepTotal verifies that the token budget is shared
// across all bindings in one Route call, not per-binding.
//
// Before the fix: each binding got its own budget, so 2 MCPs with a 500-token
// budget each got 500 tokens — total 1000.
//
// After the fix: the 500-token budget is shared, so the second MCP draws
// from whatever the first MCP left.
func TestRoute_BudgetPerStepTotal(t *testing.T) {
	// Two MCPs, each with a tool that costs ~400 tokens.
	// Budget: 500 tokens total.
	//
	// With per-binding budget: each MCP gets 500 tokens, each selects
	//   its 400-token tool. Total: 2 tools, 800 tokens.
	// With per-step total budget: first MCP selects its 400-token tool
	//   (400 ≤ 500). Remaining: 100. Second MCP's tool costs 400 > 100,
	//   so it is not selected. Total: 1 tool, 400 tokens.

	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: []string{"mcp1_tool"},
				FullToolDefinitions: []schemas.ToolDefinition{
					toolDef("mcp1_tool", 400),
				},
			},
			"mcp2": {
				ID:             "mcp2",
				AvailableTools: []string{"mcp2_tool"},
				FullToolDefinitions: []schemas.ToolDefinition{
					toolDef("mcp2_tool", 400),
				},
			},
		},
	}

	docs := map[string]string{
		"mcp1_tool": "relevant tool one",
		"mcp2_tool": "relevant tool two",
	}
	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)

	routed := router.Route(context.Background(), RouteRequest{
		Task: "relevant tool one relevant tool two",
		Bindings: []config.MCPBinding{
			{MCPID: "mcp1"},
			{MCPID: "mcp2"},
		},
		ProgressiveMode: false,
		MaxTokenBudget:  500,
	})

	// Count total tools selected across all bindings.
	totalTools := 0
	for _, b := range routed {
		totalTools += len(b.AllowedTools)
	}

	// With per-step total budget: only 1 tool should be selected (400 ≤ 500,
	// but 400+400 > 500). With per-binding budget: 2 tools would be selected.
	if totalTools > 1 {
		t.Errorf("per-step total budget should limit total tools to 1, got %d "+
			"(budget=500, each tool≈400 tokens) — budget was applied per-binding "+
			"instead of per-step total", totalTools)
	}
}
