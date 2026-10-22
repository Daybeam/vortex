package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// Regression (2026-10-20): tier was resolved inconsistently.
//
// loggedHandler injects TierAdmin into the context for stdio (no HTTP auth
// middleware), and CheckTaskOwnership reads it from the context. But
// SubsystemRegistry.Invoke and discovery.discover read the process-level
// app.Tier, which is "" on the apps built in main.go. A stdio caller was
// therefore admin for ownership and Tier1-restricted for subsystem actions.
//
// These tests pin EffectiveTier as the single resolution point AND exercise
// the real Invoke path end-to-end, so they fail if any consumer reads app.Tier
// directly again.

func TestEffectiveTier_ContextWins(t *testing.T) {
	app := &App{} // stdio construction: Tier == ""
	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)
	if got := EffectiveTier(ctx, app); got != TierAdmin {
		t.Errorf("context tier must win over app.Tier: got %q", got)
	}
}

func TestEffectiveTier_FallsBackToAppTier(t *testing.T) {
	// Paths that never traverse loggedHandler (background jobs, direct
	// handler invocation) still resolve through app.Tier.
	app := &App{Tier: TierPublic}
	if got := EffectiveTier(context.Background(), app); got != TierPublic {
		t.Errorf("no ctx tier should fall back to app.Tier: got %q", got)
	}
}

func TestEffectiveTier_ExplicitEmptyInCtxIsHonored(t *testing.T) {
	// An explicit empty tier in ctx (public app that missed the middleware)
	// must NOT silently fall back to an admin-ish app.Tier.
	ctx := context.WithValue(context.Background(), AuthTierKey, "")
	app := &App{Tier: TierAdmin}
	if got := EffectiveTier(ctx, app); got != "" {
		t.Errorf("explicit empty ctx tier must be honored, got %q", got)
	}
}

func TestEffectiveTier_NilApp(t *testing.T) {
	if got := EffectiveTier(context.Background(), nil); got != "" {
		t.Errorf("nil app should resolve to empty tier, got %q", got)
	}
}

// registerTierProbeSubsystem registers a throwaway subsystem whose only action
// is deliberately outside tier1AllowedActions. It lets the tests observe
// Invoke's real permission decision.
func registerTierProbeSubsystem(name string) {
	registry.Register(&Subsystem{
		Name: name,
		Actions: map[string]Action{
			"secret": {
				Name: "secret",
				Handler: func(context.Context, *App, map[string]any) (any, error) {
					return "reached", nil
				},
			},
		},
	})
}

// Invoke must treat a stdio caller (app.Tier == "", ctx tier == admin) as
// admin, not as Tier1. Fails if Invoke goes back to reading app.Tier.
func TestInvoke_StdioCallerIsAdminNotTier1(t *testing.T) {
	const sub = "tierprobe_a"
	registerTierProbeSubsystem(sub)

	if tier1AllowedActions[sub+".secret"] {
		t.Fatal("probe action must stay outside the Tier1 allowlist")
	}

	app := &App{} // exact stdio construction
	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	if _, err := registry.Invoke(ctx, app, sub, "secret", map[string]any{}); err != nil {
		t.Fatalf("stdio caller must be admin for subsystem actions, got: %v", err)
	}
}

// A public caller must still be denied the same action — the fix must not
// widen access.
func TestInvoke_PublicCallerStillDenied(t *testing.T) {
	const sub = "tierprobe_b"
	registerTierProbeSubsystem(sub)

	app := &App{Tier: TierPublic}
	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	if _, err := registry.Invoke(ctx, app, sub, "secret", map[string]any{}); err == nil {
		t.Fatal("SECURITY: public caller must NOT reach a non-whitelisted action")
	}
}

// discovery.discover must see isAdmin=true under stdio.
//
// This calls the REAL handler registered by registerDiscoverySubsystem. The
// handler is fetched from the registry and invoked directly (not through
// Invoke) so the assertion isolates the isAdmin computation from the
// isActionAllowed gate, which is covered by the Invoke tests above.
func TestDiscover_StdioSeesAdmin(t *testing.T) {
	app := &App{Registry: emptyRegistryForTierTest(t)} // stdio construction: Tier == ""
	registerDiscoverySubsystem(app)
	sub, ok := registry.Subsystems["discovery"]
	if !ok {
		t.Fatal("discovery subsystem was not registered")
	}
	handler := sub.Actions["discover"].Handler

	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	// type=mcps goes through enforceScope, which refuses scope=pool unless
	// AllowPool is set. AllowPool comes from isAdmin, which comes from the
	// tier. A non-admin caller therefore gets "requires admin permission".
	_, err := handler(ctx, app, map[string]any{"scope": "pool", "type": "mcps"})
	if err != nil {
		t.Fatalf("stdio caller must be admin, got: %v", err)
	}
}

// And a public caller must still be blocked from the pool scope.
//
// Uses type=mcps because discoverMCPs (like discoverTools) routes through
// enforceScope, where AllowPool is checked. See
// TestDiscoverRoles_DoesNotEnforceScope for the known gap on type=roles.
func TestDiscover_PublicCallerCannotUsePoolScope(t *testing.T) {
	app := &App{Tier: TierPublic, Registry: emptyRegistryForTierTest(t)}
	registerDiscoverySubsystem(app)
	handler := registry.Subsystems["discovery"].Actions["discover"].Handler

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	// scope=pool without AllowPool is rejected. For a public caller isAdmin is
	// false, so ParseScope yields AllowPool=false and enforceScope must refuse.
	_, err := handler(ctx, app, map[string]any{"scope": "pool", "type": "mcps"})
	if err == nil || !strings.Contains(err.Error(), "requires admin permission") {
		t.Fatalf("SECURITY: public caller must be denied scope=pool, got: %v", err)
	}
}

// FIXED (audit L-20-2): discoverRoles now enforces AllowPool for scope=pool,
// matching enforceScope (MCPs) and discoverTools. A public caller is denied.
// This replaces the prior characterization test that pinned the buggy behavior.
func TestDiscoverRoles_EnforcesAllowPool(t *testing.T) {
	app := &App{Tier: TierPublic, Registry: emptyRegistryForTierTest(t)}
	registerDiscoverySubsystem(app)
	handler := registry.Subsystems["discovery"].Actions["discover"].Handler

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	_, err := handler(ctx, app, map[string]any{"scope": "pool", "type": "roles"})
	if err == nil || !strings.Contains(err.Error(), "requires admin permission") {
		t.Fatalf("SECURITY: public caller must be denied scope=pool for roles, got: %v", err)
	}
}

// emptyRegistryForTierTest returns a Registry with no roles/MCPs so the real
// discovery handler can run end-to-end without touching disk. It only needs to
// be non-nil: DiscoverImpl locks it before reading.
func emptyRegistryForTierTest(t *testing.T) *config.Registry {
	t.Helper()
	reg := &config.Registry{}
	return reg
}
