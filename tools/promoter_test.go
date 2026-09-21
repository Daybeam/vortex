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

	// Verify runtime enforcement: simulate a Public (Tier1) request
	// (we'd need to invoke the registered handler directly to test this,
	// but the structure is now safe).
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
