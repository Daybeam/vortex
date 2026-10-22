package tools

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/core"
)

// Security regression tests for the discovery subsystem.
//
// These tests pin the fixes for the security audit (2026-10-21):
//   1. Invoke strips _-prefixed args (prevents _is_admin/_role_id injection)
//   2. ParseScope rejects role:<id> for non-admin non-self callers
//   3. Discover handler does not silently escalate self→pool for type=roles

// TestInvoke_StripsUnderscorePrefixedArgs verifies that the Invoke dispatcher
// strips all keys starting with "_" from args before passing to the handler.
// This prevents callers from injecting _is_admin, _role_id, or any other
// internal injection point.
func TestInvoke_StripsUnderscorePrefixedArgs(t *testing.T) {
	const sub = "secprobe_strip"
	var receivedArgs map[string]any

	registry.Register(&Subsystem{
		Name: sub,
		Actions: map[string]Action{
			"probe": {
				Name: "probe",
				Handler: func(_ context.Context, _ *App, args map[string]any) (any, error) {
					receivedArgs = args
					return "ok", nil
				},
			},
		},
	})

	app := &App{}
	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	_, err := registry.Invoke(ctx, app, sub, "probe", map[string]any{
		"normal_param": "visible",
		"_is_admin":    true,
		"_role_id":     "admin_role",
		"_secret":      "should_be_stripped",
	})
	if err != nil {
		t.Fatalf("Invoke failed: %v", err)
	}

	// Verify _-prefixed keys were stripped
	for k := range receivedArgs {
		if len(k) > 0 && k[0] == '_' {
			t.Errorf("SECURITY: _-prefixed key %q leaked into handler args", k)
		}
	}

	// Verify normal params survived
	if receivedArgs["normal_param"] != "visible" {
		t.Errorf("normal param was stripped: %v", receivedArgs)
	}
}

// TestDiscover_PublicCtxPoolScopeDenied verifies that a public caller
// cannot use scope=pool even if they try to inject _is_admin:true.
func TestDiscover_PublicCtxPoolScopeDenied(t *testing.T) {
	app := &App{
		Registry: emptyRegistryForTierTest(t),
		Tier:     TierPublic,
	}
	registerDiscoverySubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	// Even with _is_admin:true in args, the public caller must not get pool scope.
	// Invoke strips _-prefixed keys, and the handler reads tier only from ctx.
	_, err := registry.Invoke(ctx, app, "discovery", "discover", map[string]any{
		"scope":     "pool",
		"type":      "tools",
		"_is_admin": true, // attack attempt — must be stripped
	})
	if err == nil {
		t.Fatal("SECURITY: public caller must NOT access pool scope even with _is_admin:true")
	}
}

// TestDiscover_PublicCtxRoleScopeDenied verifies that a public caller
// cannot use scope=role:<other_role> to inspect another role's bindings.
func TestDiscover_PublicCtxRoleScopeDenied(t *testing.T) {
	app := &App{
		Registry: emptyRegistryForTierTest(t),
		Tier:     TierPublic,
	}
	registerDiscoverySubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	_, err := registry.Invoke(ctx, app, "discovery", "discover", map[string]any{
		"scope": "role:admin_role",
		"type":  "tools",
	})
	if err == nil {
		t.Fatal("SECURITY: public caller must NOT inspect another role's bindings via role:<id>")
	}
}

// TestDiscover_SelfScopeNoRoleReturnsEmpty verifies that scope=self with no
// caller role ID returns empty instead of silently escalating to pool.
func TestDiscover_SelfScopeNoRoleReturnsEmpty(t *testing.T) {
	app := &App{
		Registry: emptyRegistryForTierTest(t),
		Tier:     TierPublic,
	}
	registerDiscoverySubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	result, err := registry.Invoke(ctx, app, "discovery", "discover", map[string]any{
		"scope": "self",
		"type":  "roles",
	})
	if err != nil {
		t.Fatalf("self scope should not error: %v", err)
	}

	// Must return empty, NOT a pool-level list of all roles
	dr, ok := result.(*core.DiscoveryResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if dr.Total != 0 {
		t.Errorf("SECURITY: self scope with no role must return empty, got total=%d (pool escalation!)", dr.Total)
	}
}
