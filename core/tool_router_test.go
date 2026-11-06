/*
 * ToolRouter SearchTools — keyword-based tool search with pagination.
 * Also: R1 (BM25 corpus leakage) and R4 (dedup loop + deterministic sort)
 * regression tests, previously in tool_router_r1_test.go and
 * tool_router_r4_test.go — merged 2026-10-31. The R1/R4 refactor-round IDs
 * are preserved in the section comments below.
 */

package core

import (
	"context"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"sync"
	"testing"
)

func TestSearchTools(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "get_earnings", Description: "Get earnings data"},
					{Name: "earnings_report", Description: "Get earnings report"},
				},
			},
		},
	}

	router := NewToolRouter(registry, nil)

	// Test 1: Successful search (roleID="" → search all MCPs)
	res, err := router.SearchTools(context.Background(), "earnings", "", "", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(res.Tools))
	}

	// Test 2: Pagination
	res, err = router.SearchTools(context.Background(), "earnings", "", "", 1, 1)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(res.Tools))
	}

	// Test 3: Zero results
	res, err = router.SearchTools(context.Background(), "unknown", "", "", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(res.Tools))
	}
}

// D3 fix (2026-10-20): SearchTools with roleID must scope to the role's bound
// MCPs only. A role with no bindings gets empty results (no pool fallback).
// See CUSTOMER_V010_AUDIT_2026-10-20.md §D3.
func TestSearchTools_RoleScoped(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "get_earnings", Description: "Get earnings data"},
				},
			},
			"mcp2": {
				ID: "mcp2",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "send_email", Description: "Send an email"},
				},
			},
		},
		Roles: map[string]*config.Role{
			"role_with_mcp1": {
				ID:               "role_with_mcp1",
				BoundMCPBindings: []config.MCPBinding{{MCPID: "mcp1"}},
			},
			"role_no_bindings": {
				ID: "role_no_bindings",
			},
		},
	}

	router := NewToolRouter(registry, nil)

	// Role bound to mcp1 → can search mcp1's tools, NOT mcp2's.
	res, err := router.SearchTools(context.Background(), "", "", "role_with_mcp1", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "get_earnings" {
		t.Fatalf("expected only get_earnings from mcp1, got %v", res.Tools)
	}

	// Role bound to mcp1 → cannot find mcp2's tools.
	res, err = router.SearchTools(context.Background(), "email", "", "role_with_mcp1", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools (email is in mcp2, not bound to role), got %v", res.Tools)
	}

	// Role with no bindings → empty results (no pool fallback).
	res, err = router.SearchTools(context.Background(), "", "", "role_no_bindings", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools for role with no bindings, got %d", len(res.Tools))
	}

	// Non-existent role → empty results.
	res, err = router.SearchTools(context.Background(), "", "", "nonexistent", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools for nonexistent role, got %d", len(res.Tools))
	}
}

// ─── R1: BM25 corpus leakage ────────────────────────────────────────────────
//
// Bug: corpus.Score(req.Task) scores the ENTIRE corpus, not just the
// available tools for the current binding. After RRF merge, a tool that
// is in the corpus but NOT in the binding's allowlist can appear in the
// routed output — a security violation. Fix: filter bm25Rank to available.

func TestRoute_R1_BM25DoesNotLeakOutsideAvailable(t *testing.T) {
	allTools := []string{"tool_a", "tool_b", "tool_c", "tool_d", "tool_e"}
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
			"mcp1": {ID: "mcp1", AvailableTools: allTools},
		},
	}
	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)
	bindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a", "tool_b", "tool_c"}},
	}
	req := RouteRequest{Task: "delta special unique keyword match", Bindings: bindings}
	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}
	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}
	if allowed["tool_d"] {
		t.Error("tool_d leaked — R1: BM25 scored entire corpus, unauthorized tool entered RRF merge")
	}
	authorized := map[string]bool{"tool_a": true, "tool_b": true, "tool_c": true}
	for tool := range allowed {
		if !authorized[tool] {
			t.Errorf("routed tool %q not in authorized set — R1: output ⊄ authorized", tool)
		}
	}
}

func TestRoute_R1_BM25DoesNotLeakUnrestricted(t *testing.T) {
	docs := map[string]string{
		"authorized_1": "first authorized tool",
		"authorized_2": "second authorized tool",
		"stale_tool":   "stale tool still in corpus but removed from MCP",
	}
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {ID: "mcp1", AvailableTools: []string{"authorized_1", "authorized_2"}},
		},
	}
	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)
	bindings := []config.MCPBinding{{MCPID: "mcp1"}}
	req := RouteRequest{Task: "stale tool still in corpus but removed from MCP", Bindings: bindings}
	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}
	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}
	if allowed["stale_tool"] {
		t.Error("stale_tool leaked — R1: tool not in AvailableTools entered RRF merge")
	}
}

// ─── R4: dedup loop termination + deterministic sort ────────────────────────
//
// Bug A: selection loop exited on redundant tool, dropping all subsequent.
// Bug B: cosine dedup (>0.85) killed paired tools with near-identical embeddings.
// Fix: removed embedding dedup; deterministic sort (score desc, id asc).

func TestRoute_R4_PairedToolsNotDroppedByDedup(t *testing.T) {
	tools := []string{
		"venmo__show_sent_payment_requests",
		"venmo__show_received_payment_requests",
		"venmo__send_payment", "venmo__get_balance", "venmo__show_profile",
		"venmo__add_note", "venmo__get_user",
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
			"mcp1": {ID: "mcp1", AvailableTools: tools},
		},
	}
	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)
	pairedVec := []float32{1.0, 0.5, 0.3}
	router.embeddings = map[string][]float32{
		"venmo__show_sent_payment_requests":     pairedVec,
		"venmo__show_received_payment_requests": pairedVec,
		"venmo__send_payment":                   {0.8, 0.2, 0.1},
		"venmo__get_balance":                    {0.1, 0.9, 0.0},
		"venmo__show_profile":                   {0.3, 0.3, 0.9},
		"venmo__add_note":                       {0.0, 0.1, 0.8},
		"venmo__get_user":                       {0.2, 0.0, 0.7},
	}
	bindings := []config.MCPBinding{{MCPID: "mcp1"}}
	req := RouteRequest{Task: "show sent and received payment requests", Bindings: bindings}
	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}
	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}
	if !allowed["venmo__show_sent_payment_requests"] {
		t.Error("venmo__show_sent_payment_requests dropped — R4 Bug B: cosine dedup killed paired tool")
	}
	if !allowed["venmo__show_received_payment_requests"] {
		t.Error("venmo__show_received_payment_requests dropped — R4 Bug B: cosine dedup killed paired tool")
	}
	if len(routed[0].AllowedTools) < 5 {
		t.Errorf("expected ≥5 routed tools, got %d — R4 Bug A: loop terminated early", len(routed[0].AllowedTools))
	}
}

func TestRoute_R4_DeterministicSortEqualScores(t *testing.T) {
	tools := []string{"app__delta", "app__alpha", "app__epsilon", "app__beta", "app__gamma"}
	docs := make(map[string]string)
	for _, tool := range tools {
		docs[tool] = "common test tool"
	}
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {ID: "mcp1", AvailableTools: tools},
		},
	}
	router := NewToolRouter(registry, nil)
	router.BM25Corpus = NewBM25Corpus(1.2, 0.75, docs)
	bindings := []config.MCPBinding{{MCPID: "mcp1"}}
	req := RouteRequest{Task: "common test tool", Bindings: bindings}
	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}
	got := routed[0].AllowedTools
	if len(got) != 5 {
		t.Fatalf("expected 5 routed tools, got %d: %v", len(got), got)
	}
	expected := []string{"app__alpha", "app__beta", "app__delta", "app__epsilon", "app__gamma"}
	for i, want := range expected {
		if got[i] != want {
			t.Errorf("position %d: expected %q, got %q — R4: non-deterministic sort", i, want, got[i])
		}
	}
}

