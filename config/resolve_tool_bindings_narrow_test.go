// Regression test for per-step tool allowlists on role-bound MCPs (2026-10-18).
//
// Bug being pinned: Registry.ResolveToolBindings only inserted an `extra`
// binding when the MCP was NOT already bound to the role. appworld_agent binds
// `appworld` with AllowDynamicMCPs=false, so the planner-provided
// AdditionalToolAllowlists map was silently dropped and every step ran with
// the role's entire tool surface. The planner's explicit tool selection — the
// whole point of the 2026-10-18 design — had no effect.
//
// Contract: extra may NARROW a bound MCP, never widen it, and an empty
// AllowedTools on the extra binding means "unspecified", not "allow all".

package config

import (
	"testing"
)

func probeRole() *Role {
	return &Role{
		ID:               "probe_agent",
		AllowDynamicMCPs: false,
		BoundMCPBindings: []MCPBinding{
			{
				MCPID:         "appworld",
				AllowedTools:  []string{"a__login", "a__logout", "a__x", "a__y", "supervisor__complete_task"},
				AlwaysInclude: []string{"supervisor__complete_task"},
			},
		},
	}
}

// TestResolveToolBindings_ExtraNarrowsBoundMCP is the core regression: prior
// behaviour returned the role's 5 tools, ignoring the extra list entirely.
func TestResolveToolBindings_ExtraNarrowsBoundMCP(t *testing.T) {
	r := &Registry{}
	got := r.ResolveToolBindings(probeRole(), []MCPBinding{
		{MCPID: "appworld", AllowedTools: []string{"a__x", "a__y"}},
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 binding, got %d: %+v", len(got), got)
	}
	b := got[0]
	if len(b.AllowedTools) != 2 {
		t.Fatalf("BUG REPRODUCED: extra allowlist was dropped; got %d tools %v (want exactly 2)",
			len(b.AllowedTools), b.AllowedTools)
	}
	for _, want := range []string{"a__x", "a__y"} {
		found := false
		for _, have := range b.AllowedTools {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Errorf("narrowed allowlist missing %q: %v", want, b.AllowedTools)
		}
	}
	// Narrowing must remove the tools the caller deliberately left out.
	for _, unwanted := range []string{"a__login", "a__logout"} {
		for _, have := range b.AllowedTools {
			if have == unwanted {
				t.Errorf("narrowing failed to remove %q: %v", unwanted, b.AllowedTools)
			}
		}
	}
}

// TestResolveToolBindings_AlwaysIncludePreserved guards the role-level safety
// net: even after the caller narrows a bound MCP, tools the role marks
// AlwaysInclude must survive, otherwise a step could lose its ability to
// signal completion.
func TestResolveToolBindings_AlwaysIncludePreserved(t *testing.T) {
	r := &Registry{}
	got := r.ResolveToolBindings(probeRole(), []MCPBinding{
		{MCPID: "appworld", AllowedTools: []string{"a__x"}},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(got))
	}
	if len(got[0].AlwaysInclude) == 0 || got[0].AlwaysInclude[0] != "supervisor__complete_task" {
		t.Fatalf("AlwaysInclude lost during narrowing: %+v", got[0])
	}
}

// TestResolveToolBindings_EmptyExtraDoesNotWiden makes sure an empty extra
// allowlist cannot be read as "allow everything". It must be a no-op.
func TestResolveToolBindings_EmptyExtraDoesNotWiden(t *testing.T) {
	r := &Registry{}
	got := r.ResolveToolBindings(probeRole(), []MCPBinding{
		{MCPID: "appworld", AllowedTools: nil},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(got))
	}
	if len(got[0].AllowedTools) != 5 {
		t.Fatalf("empty extra allowlist must not change the binding; got %v", got[0].AllowedTools)
	}
}

// TestResolveToolBindings_UnboundExtraAdded keeps the original behaviour for
// MCPs the role does not bind.
func TestResolveToolBindings_UnboundExtraAdded(t *testing.T) {
	r := &Registry{}
	got := r.ResolveToolBindings(probeRole(), []MCPBinding{
		{MCPID: "phone", AllowedTools: []string{"search_contacts"}},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 bindings (appworld + phone), got %d: %+v", len(got), got)
	}
	var phone *MCPBinding
	for i := range got {
		if got[i].MCPID == "phone" {
			phone = &got[i]
		}
	}
	if phone == nil {
		t.Fatalf("phone binding missing")
	}
	if len(phone.AllowedTools) != 1 || phone.AllowedTools[0] != "search_contacts" {
		t.Fatalf("unbound extra allowlist mangled: %v", phone.AllowedTools)
	}
}
