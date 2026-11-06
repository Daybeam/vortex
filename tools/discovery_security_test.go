package tools

import (
	"github.com/daybeam/vortex/core/capability"
	"context"
	"strings"
	"testing"

)

// Security regression tests for the discovery subsystem.
//
// These tests pin the fixes for the security audit (2026-10-21):
//   1. Invoke strips _-prefixed args (prevents _is_admin/_role_id injection);
//      the one exception is the server-injected _actor (audit O4, 2026-11-04)
//   2. ParseScope rejects role:<id> for non-admin non-self callers
//   3. Discover handler does not silently escalate self→pool for type=roles

// TestInvoke_StripsUnderscorePrefixedArgs verifies that the Invoke dispatcher
// strips all keys starting with "_" from args before passing to the handler.
// This prevents callers from injecting _is_admin, _role_id, or any other
// internal injection point.
//
// audit O4 (2026-11-04) added one server-injected key, so the invariant is now
// "no *caller-controlled* _-prefixed key reaches a handler": _actor may appear,
// and only ever with the value resolved from the context. Both halves are
// pinned below, so the injection cannot be quietly widened later.
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
	forged := map[string]any{
		"normal_param": "visible",
		"_is_admin":    true,
		"_role_id":     "admin_role",
		"_secret":      "should_be_stripped",
		"_actor":       "attacker-chosen",
	}

	// Case 1: admin with no owner and no client session, so the server has no
	// identity to attribute -- nothing may be injected, and every _-prefixed key
	// must be gone.
	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)
	if _, err := registry.Invoke(ctx, app, sub, "probe", forged); err != nil {
		t.Fatalf("Invoke failed: %v", err)
	}
	for k := range receivedArgs {
		if strings.HasPrefix(k, "_") {
			t.Errorf("SECURITY: _-prefixed key %q leaked into handler args", k)
		}
	}

	// Verify normal params survived
	if receivedArgs["normal_param"] != "visible" {
		t.Errorf("normal param was stripped: %v", receivedArgs)
	}

	// Case 2: the same forged body from a caller the server *can* identify. The
	// only surviving _-key must be _actor, and it must carry the context value,
	// not the caller's.
	if _, err := registry.Invoke(context.WithValue(ctx, AuthOwnerKey, "tenant-a"), app, sub, "probe", forged); err != nil {
		t.Fatalf("Invoke with owner: %v", err)
	}
	if got := receivedArgs["_actor"]; got != "tenant-a" {
		t.Errorf("SECURITY: _actor = %v, want the context identity %q", got, "tenant-a")
	}
	for k := range receivedArgs {
		if strings.HasPrefix(k, "_") && k != "_actor" {
			t.Errorf("SECURITY: caller-controlled _-prefixed key %q reached the handler", k)
		}
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
	dr, ok := result.(*capability.DiscoveryResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if dr.Total != 0 {
		t.Errorf("SECURITY: self scope with no role must return empty, got total=%d (pool escalation!)", dr.Total)
	}
}
