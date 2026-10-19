package tools

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// Regression for docs/53-appworld-transport-fix-20261018.md §4:
// stdio connections don't go through HTTP auth middleware, so AuthTierKey
// is never set in the context. This causes cross_owner_denied because
// CheckTaskOwnership sees tier="" (not admin) and the caller identity
// doesn't match the task owner.
//
// Fix: loggedHandler injects TierAdmin when AuthTierKey is not set.
// stdio is a trusted local pipe — this matches the "Dev Mode (No keys
// configured)" semantic in main_web.go:855-861.

func TestLoggedHandler_StdioInjectsAdminTier(t *testing.T) {
	// Simulate a stdio connection: no AuthTierKey in context.
	// The handler should inject TierAdmin so ownership checks pass.
	app := &App{} // Logger is nil → early return path

	var capturedTier string
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v, ok := ctx.Value(AuthTierKey).(string); ok {
			capturedTier = v
		}
		return &mcp.CallToolResult{}, nil
	}

	wrapped := app.loggedHandler("test_tool", handler)
	// stdio context: no AuthTierKey set
	ctx := context.Background()
	wrapped(ctx, mcp.CallToolRequest{})

	if capturedTier != TierAdmin {
		t.Errorf("stdio connection (no AuthTierKey) should get TierAdmin injected, got %q", capturedTier)
	}
}

func TestLoggedHandler_HTTPDoesNotOverrideTier(t *testing.T) {
	// Simulate an HTTP public-tier connection: AuthTierKey=TierPublic.
	// The handler should NOT override it to TierAdmin.
	app := &App{}

	var capturedTier string
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v, ok := ctx.Value(AuthTierKey).(string); ok {
			capturedTier = v
		}
		return &mcp.CallToolResult{}, nil
	}

	wrapped := app.loggedHandler("test_tool", handler)
	// HTTP public context: AuthTierKey=TierPublic
	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)
	wrapped(ctx, mcp.CallToolRequest{})

	if capturedTier != TierPublic {
		t.Errorf("HTTP public tier should NOT be overridden to admin, got %q", capturedTier)
	}
}

func TestLoggedHandler_HTTPAdminTierPreserved(t *testing.T) {
	// Simulate an HTTP admin-tier connection: AuthTierKey=TierAdmin.
	// The handler should preserve it.
	app := &App{}

	var capturedTier string
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v, ok := ctx.Value(AuthTierKey).(string); ok {
			capturedTier = v
		}
		return &mcp.CallToolResult{}, nil
	}

	wrapped := app.loggedHandler("test_tool", handler)
	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)
	wrapped(ctx, mcp.CallToolRequest{})

	if capturedTier != TierAdmin {
		t.Errorf("HTTP admin tier should be preserved, got %q", capturedTier)
	}
}

func TestCheckTaskOwnership_StdioAdminBypassesOwnerCheck(t *testing.T) {
	// After the fix, stdio connections have TierAdmin in context.
	// CheckTaskOwnership should return true regardless of task owner.
	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	// Task owned by "stdio" (from SubmitWithSessionIR), caller identity is ""
	// (no session ID in subsequent stdio calls). With admin tier, this should pass.
	if !CheckTaskOwnership(ctx, "stdio") {
		t.Errorf("admin tier should bypass ownership check: taskOwner='stdio' should be accessible")
	}

	// Even a task owned by a completely different identity should pass
	if !CheckTaskOwnership(ctx, "some_other_owner") {
		t.Errorf("admin tier should bypass ownership check for any task owner")
	}
}

func TestCheckTaskOwnership_StdioWithoutFixFails(t *testing.T) {
	// This documents the BUG: without the fix, stdio has no AuthTierKey.
	// CheckTaskOwnership fails when task owner != caller identity.
	// This test pins the pre-fix behavior so we know the fix is load-bearing.
	ctx := context.Background() // no AuthTierKey — simulates pre-fix stdio

	// Task owner is "stdio", caller identity is "" → should fail
	if CheckTaskOwnership(ctx, "stdio") {
		t.Errorf("pre-fix stdio (no tier) should FAIL ownership check for taskOwner='stdio'")
	}
}

// Regression for security fix (2026-10-19):
// loggedHandler wrapped ALL transports, not just stdio. If an SSE/HTTP path
// failed to set AuthTierKey, the request silently became admin → cross-tenant
// access. Fix: only inject TierAdmin when app.Tier != TierPublic.
//
// See audit report §2: "769d16808 的默认 admin 注入".

func TestLoggedHandler_PublicAppNoTierDoesNotGetAdmin(t *testing.T) {
	// A public app (app.Tier == TierPublic) with a missing AuthTierKey
	// (e.g., an SSE path that forgot to set the tier) must NOT get TierAdmin
	// injected. Before the fix, this silently escalated to admin → cross-tenant.
	app := &App{Tier: TierPublic}

	var capturedTier string
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v, ok := ctx.Value(AuthTierKey).(string); ok {
			capturedTier = v
		}
		return &mcp.CallToolResult{}, nil
	}

	wrapped := app.loggedHandler("test_tool", handler)
	// No AuthTierKey set — simulates a missed SSE/HTTP path
	ctx := context.Background()
	wrapped(ctx, mcp.CallToolRequest{})

	if capturedTier == TierAdmin {
		t.Fatal("SECURITY: public app with missing AuthTierKey got TierAdmin injected — cross-tenant access possible")
	}
	if capturedTier != "" {
		t.Errorf("public app with missing AuthTierKey should have empty tier (safe denial), got %q", capturedTier)
	}
}

func TestLoggedHandler_AdminAppNoTierGetsAdmin(t *testing.T) {
	// A non-public app (e.g., stdio harness, app.Tier == "" or TierAdmin)
	// with a missing AuthTierKey should still get TierAdmin injected.
	// This preserves the stdio trusted-pipe behavior.
	app := &App{Tier: ""} // admin/default app

	var capturedTier string
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v, ok := ctx.Value(AuthTierKey).(string); ok {
			capturedTier = v
		}
		return &mcp.CallToolResult{}, nil
	}

	wrapped := app.loggedHandler("test_tool", handler)
	ctx := context.Background()
	wrapped(ctx, mcp.CallToolRequest{})

	if capturedTier != TierAdmin {
		t.Errorf("admin/default app with missing AuthTierKey should get TierAdmin injected, got %q", capturedTier)
	}
}
