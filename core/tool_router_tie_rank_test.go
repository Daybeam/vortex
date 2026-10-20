// Regression tests for RRF tie handling and the schema-budget selection cap.
//
// Both pin invariants of the router that are independent of any particular tool
// set, naming convention or task domain:
//
//  1. A channel that assigns the same score to every member of a tool family
//     carries no ordering information. RRF is rank-based, so if those members
//     are handed sequential ranks the fused order collapses to the tie-break
//     key (id ascending) and the ranking is decided alphabetically instead of
//     by the channel that can actually discriminate.
//
//  2. The selection cap is a token budget, not a tool count. A count cannot
//     express affordability (five verbose schemas can cost more than forty
//     terse ones), and any fixed count silently makes tools unreachable.

package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// billingFamily returns a tool set whose members all share the token "billing",
// so the task text "…the billing service…" makes the string channel fire flat
// across the whole family. The discriminating evidence lives only in
// billing__void_bill, which is also the alphabetically last member.
func billingFamily() ([]string, map[string]string) {
	tools := []string{
		"billing__add_account",
		"billing__add_card",
		"billing__archive_bill",
		"billing__create_bill",
		"billing__delete_bill",
		"billing__list_bills",
		"billing__pay_bill",
		"billing__refund_bill",
		"billing__show_account",
		"billing__show_bill",
		"billing__update_bill",
		"billing__void_bill",
	}
	docs := make(map[string]string, len(tools))
	for _, tool := range tools {
		words := strings.ReplaceAll(strings.TrimPrefix(tool, "billing__"), "_", " ")
		docs[tool] = "[Tool:" + tool + "] billing " + words
	}
	return tools, docs
}

func newBillingRouter(tools []string, docs map[string]string) *ToolRouter {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"billing": {
				ID:             "billing",
				AvailableTools: tools,
			},
		},
	}
	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)
	return router
}

// TestRoute_FlatMatchFamilyOrderedByBM25 verifies that a tool family matched
// uniformly by the string channel is ordered by the channel that discriminates
// (BM25), not by tool-name alphabetical position.
//
// Before tie-aware RRF: the strongest member (billing__void_bill) received the
// worst string-channel rank purely because its name sorts last, and lost to
// alphabetically early members with no lexical evidence for the action at all.
func TestRoute_FlatMatchFamilyOrderedByBM25(t *testing.T) {
	tools, docs := billingFamily()
	router := newBillingRouter(tools, docs)

	routed := router.Route(context.Background(), RouteRequest{
		Task:            "In the billing service, void the bill",
		Bindings:        []config.MCPBinding{{MCPID: "billing"}},
		ProgressiveMode: false,
	})
	if len(routed) == 0 || len(routed[0].AllowedTools) == 0 {
		t.Fatal("expected the router to select at least one tool")
	}

	got := routed[0].AllowedTools[0]
	if got != "billing__void_bill" {
		t.Errorf("strongest lexical evidence must rank first, got %q (full order: %v)",
			got, routed[0].AllowedTools)
	}
}

// TestRoute_RespectsTokenBudget verifies the cap is driven by schema size: a
// tighter budget admits fewer tools, and the first tool is always admitted so
// that one oversized schema cannot produce an empty surface.
func TestRoute_RespectsTokenBudget(t *testing.T) {
	tools, docs := billingFamily()

	countFor := func(budget int) int {
		router := newBillingRouter(tools, docs)
		routed := router.Route(context.Background(), RouteRequest{
			Task:            "In the billing service, void the bill",
			Bindings:        []config.MCPBinding{{MCPID: "billing"}},
			ProgressiveMode: false,
			MaxTokenBudget:  budget,
		})
		if len(routed) == 0 {
			t.Fatalf("budget=%d produced no routed binding", budget)
		}
		return len(routed[0].AllowedTools)
	}

	tight := countFor(40)
	loose := countFor(8000)

	if tight < 1 {
		t.Fatalf("even an over-tight budget must admit one tool, got %d", tight)
	}
	if tight >= loose {
		t.Errorf("a tighter budget must admit fewer tools: tight=%d loose=%d", tight, loose)
	}
	if loose != len(tools) {
		t.Errorf("a budget that fits every schema must admit all %d tools, got %d", len(tools), loose)
	}
}

// TestRoute_NoFixedToolCount verifies the router no longer truncates a relevant
// tool set to a hardcoded count when no budget is configured. A fixed count is
// a reachability hazard: every tool beyond it is invisible to the agent, and a
// tool the agent never sees cannot be recovered from by anything the model does.
func TestRoute_NoFixedToolCount(t *testing.T) {
	tools, docs := billingFamily()
	router := newBillingRouter(tools, docs)

	routed := router.Route(context.Background(), RouteRequest{
		Task:            "In the billing service, void the bill",
		Bindings:        []config.MCPBinding{{MCPID: "billing"}},
		ProgressiveMode: false,
	})
	if len(routed) == 0 {
		t.Fatal("expected a routed binding")
	}
	if got := len(routed[0].AllowedTools); got != len(tools) {
		t.Errorf("uncapped run must admit all %d relevant tools, got %d: %v",
			len(tools), got, routed[0].AllowedTools)
	}
}
