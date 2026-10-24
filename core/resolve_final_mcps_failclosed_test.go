package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestResolveFinalMCPs_EmptyIntersectionFailsClosedForNonAdmin is the regression
// test for AUTH-4 / G6 (2026-10-23). When discovery has populated AvailableTools
// but NONE of the caller's requested tools exist there, the intersection is
// empty. Previously that fell back to allowed = [] — which the "empty =
// unrestricted" convention reads as FULL ACCESS — so a planner naming only
// non-existent tools silently granted the whole MCP surface. For a non-admin
// submitter it now fails closed; an admin keeps the permissive fallback.
func TestResolveFinalMCPs_EmptyIntersectionFailsClosedForNonAdmin(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"m": {ID: "m", MountPolicy: config.MountPolicyOpen, AvailableTools: []string{"real_tool"}},
		},
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", AllowDynamicMCPs: true},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	// The caller requests only a tool the server does not have; the MCP is a
	// dynamic addition (a bound MCP ignores per-step allowlists — see
	// validator_allowlist_scope_test.go).
	ghostAllowlist := map[string][]string{"m": {"ghost_tool"}}

	// Non-admin: must fail closed, not be silently granted full access.
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"m"}, ghostAllowlist, false); err == nil {
		t.Fatal("a non-admin submitter whose requested tools all do not exist must be rejected, not granted full access")
	}

	// Admin: keeps the permissive fallback (trusted tier), no error.
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"m"}, ghostAllowlist, true); err != nil {
		t.Fatalf("admin tier must keep the permissive fallback, got %v", err)
	}
}
