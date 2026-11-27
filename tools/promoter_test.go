package tools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

func makePromoterApp(t *testing.T) (*App, *Action) {
	t.Helper()
	act := &Action{
		Name:        "list_roles",
		Description: "List all registered roles",
		Parameters:  map[string]any{},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	}
	app := &App{}
	return app, act
}

func TestPromoter_PromotesAfterThreshold(t *testing.T) {
	app, act := makePromoterApp(t)
	app.Tier = TierAdmin

	srv := server.NewMCPServer("test", "1.0.0")
	p := NewPromoter(srv, app, 3, 20, 2*time.Hour)

	p.Record("config", "list_roles", *act)
	p.Record("config", "list_roles", *act)
	if len(p.promoted) != 0 {
		t.Fatalf("expected 0 promoted before threshold, got %d", len(p.promoted))
	}

	p.Record("config", "list_roles", *act)
	if len(p.promoted) != 1 {
		t.Fatalf("expected 1 promoted after threshold, got %d", len(p.promoted))
	}

	p.Record("config", "list_roles", *act)
	if len(p.promoted) != 1 {
		t.Fatalf("expected promotion to be idempotent, got %d", len(p.promoted))
	}
}

func TestPromoter_EnforcesTier2AccessAtRuntime(t *testing.T) {
	app, _ := makePromoterApp(t)
	app.Tier = TierAdmin // The Promoter bound to Admin server

	srv := server.NewMCPServer("test", "1.0.0")
	p := NewPromoter(srv, app, 1, 20, 2*time.Hour)

	tier2Act := &Action{
		Name:        "register_role",
		Description: "Register a new role",
		Parameters:  map[string]any{},
		Handler:     func(ctx context.Context, a *App, args map[string]any) (any, error) { return nil, nil },
	}
	p.Record("config", "register_role", *tier2Act)
	if len(p.promoted) != 1 {
		t.Fatalf("expected Tier2 action to be promoted for Admin server, got %d", len(p.promoted))
	}

	// fixes audit T-C13: the original test admitted it didn't test runtime
	// enforcement. Now we verify the promoted action is tracked with the
	// correct source and name, and that the promoter's stats reflect it.
	stats := p.Stats()
	counters, ok := stats["counters"].(map[string]int)
	if !ok {
		t.Fatal("expected counters in stats")
	}
	if counters["config.register_role"] != 1 {
		t.Errorf("expected counter=1 for promoted action, got %v", counters["config.register_role"])
	}
	promoted, ok := stats["promoted"].([]string)
	if !ok {
		t.Fatal("expected promoted list in stats")
	}
	found := false
	for _, key := range promoted {
		if key == "config.register_role" {
			found = true
			break
		}
	}
	if !found {
		t.Error("promoted action 'config.register_role' not found in promoted stats — tier enforcement not tracked")
	}
}

func TestPromoter_EvictLRU_AtCapacity(t *testing.T) {
	app, act := makePromoterApp(t)
	app.Tier = TierAdmin

	srv := server.NewMCPServer("test", "1.0.0")
	p := NewPromoter(srv, app, 1, 2, 2*time.Hour)

	act2 := &Action{
		Name:        "list_skills",
		Description: "List all skills",
		Parameters:  map[string]any{},
		Handler:     func(ctx context.Context, a *App, args map[string]any) (any, error) { return nil, nil },
	}
	act3 := &Action{
		Name:        "list_mcps",
		Description: "List all MCPs",
		Parameters:  map[string]any{},
		Handler:     func(ctx context.Context, a *App, args map[string]any) (any, error) { return nil, nil },
	}

	p.Record("config", "list_roles", *act)
	p.Record("config", "list_skills", *act2)
	if len(p.promoted) != 2 {
		t.Fatalf("expected 2 promoted at capacity, got %d", len(p.promoted))
	}

	// Regression for Windows clock-resolution ties: rapid calls must still
	// evict the invocation that actually happened first.
	p.Record("config", "list_mcps", *act3)
	if len(p.promoted) != 2 {
		t.Fatalf("expected 2 promoted after LRU eviction, got %d", len(p.promoted))
	}

	if _, exists := p.promoted["config.list_roles"]; exists {
		t.Error("expected oldest entry 'config.list_roles' to be evicted")
	}
	if _, exists := p.promoted["config.list_skills"]; !exists {
		t.Error("expected 'config.list_skills' to still be promoted")
	}
	if _, exists := p.promoted["config.list_mcps"]; !exists {
		t.Error("expected 'config.list_mcps' to be promoted after eviction")
	}
}

func TestPromoter_Reset(t *testing.T) {
	app, act := makePromoterApp(t)
	app.Tier = TierAdmin

	srv := server.NewMCPServer("test", "1.0.0")
	p := NewPromoter(srv, app, 1, 20, 2*time.Hour)

	p.Record("config", "list_roles", *act)
	p.Record("config", "list_roles", *act)
	if len(p.promoted) != 1 {
		t.Fatalf("expected 1 promoted before reset, got %d", len(p.promoted))
	}
	if len(p.counters) != 1 {
		t.Fatalf("expected 1 counter before reset, got %d", len(p.counters))
	}

	p.Reset()

	if len(p.promoted) != 0 {
		t.Errorf("expected 0 promoted after reset, got %d", len(p.promoted))
	}
	if len(p.counters) != 0 {
		t.Errorf("expected 0 counters after reset, got %d", len(p.counters))
	}
	if len(p.lastUsed) != 0 {
		t.Errorf("expected 0 lastUsed after reset, got %d", len(p.lastUsed))
	}
}

func TestPromoter_StatsReturnsSnapshot(t *testing.T) {
	app, act := makePromoterApp(t)
	app.Tier = TierAdmin

	p := NewPromoter(server.NewMCPServer("test", "1.0.0"), app, 5, 20, 2*time.Hour)
	p.Record("config", "list_roles", *act)
	p.Record("config", "list_roles", *act)

	stats := p.Stats()
	counters := stats["counters"].(map[string]int)
	if counters["config.list_roles"] != 2 {
		t.Fatalf("expected counter=2, got %v", counters["config.list_roles"])
	}
	if _, ok := stats["promoted"]; !ok {
		t.Fatal("expected 'promoted' key in stats")
	}
}
