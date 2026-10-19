package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// ─── Why this file exists ─────────────────────────────────────────────────
//
// docs 52-planner-wiring-design.md §10.2 found that a dynamically generated
// role (core/role_generator.go GenerateRoleObjects) is constructed with:
//
//	BoundMCPBindings:   []config.MCPBinding{}
//	AllowDynamicMCPs:   true
//
// On a deployment whose MCPs are declared STATICALLY in config.json (which is
// every orchestrator deployment, and specifically AppWorld), that combination
// yields an empty tool surface: ResolveToolBindings only widens an
// AllowDynamicMCPs role with registry.DynamicMCPs, and DynamicMCPs is
// populated exclusively by runtime mounting (capability.mount / JIT), never
// by config.json parsing.
//
// These tests pin that behavior at the ResolveToolBindings layer, where it is
// deterministic and needs no LLM. They are BEHAVIOR LOCK tests: they assert
// what the code does today, not what it should do. If a template-cloning fix
// lands (see §10.5), the "generated role" case below MUST start returning the
// static binding — that failure is the signal to update this test, not a
// regression.

// newGeneratedRoleShape mirrors the exact Role literal built by
// GenerateRoleObjects (core/role_generator.go, the `newRole := &config.Role{`
// block). Keep the two in sync.
func newGeneratedRoleShape() *config.Role {
	return &config.Role{
		ID:                  "generated_analyst",
		Name:                "Generated Analyst",
		BaseCapability:      "analysis",
		BoundSkills:         []string{"skill_base_generated_analyst"},
		BoundMCPBindings:    []config.MCPBinding{},
		AllowDynamicSkills:  true,
		AllowDynamicMCPs:    true,
		MaxAdditionalSkills: 5,
		Metadata:            make(map[string]string),
	}
}

// TestGenerateRoleObjects_ToolSurface_StaticMCPNotInherited locks the gap:
// a generated role sees NOTHING on a deployment that declares MCPs statically.
func TestGenerateRoleObjects_ToolSurface_StaticMCPNotInherited(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID:             "appworld",
				Command:        "python",
				AvailableTools: []string{"venmo__login", "venmo__create_payment_request"},
			},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	bindings := reg.ResolveToolBindings(newGeneratedRoleShape(), nil)

	if len(bindings) != 0 {
		t.Fatalf("BEHAVIOR CHANGED: generated role now inherits %d static MCP binding(s); "+
			"if template-cloning landed (docs 52 §10.5), update this test to assert the "+
			"inherited surface instead. got: %+v", len(bindings), bindings)
	}
}

// TestResolveToolBindings_AllowDynamicMCPsWidensWithDynamicOnly shows WHY the
// test above is empty: AllowDynamicMCPs=true does widen the binding set, but
// only from DynamicMCPs. A statically configured MCP is never picked up.
func TestResolveToolBindings_AllowDynamicMCPsWidensWithDynamicOnly(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {ID: "appworld", Command: "python"},
		},
		DynamicMCPs: map[string]*config.MCPDef{
			"mounted_api": {ID: "mounted_api", URL: "https://example.invalid/sse"},
		},
	}

	bindings := reg.ResolveToolBindings(newGeneratedRoleShape(), nil)

	byID := make(map[string]config.MCPBinding, len(bindings))
	for _, b := range bindings {
		byID[b.MCPID] = b
	}

	if _, ok := byID["mounted_api"]; !ok {
		t.Errorf("AllowDynamicMCPs should widen to DynamicMCPs entries; mounted_api missing from %+v", bindings)
	}
	if _, ok := byID["appworld"]; ok {
		t.Errorf("static config.json MCPs must NOT be inherited via AllowDynamicMCPs; appworld leaked into %+v", bindings)
	}
}

// TestGenerateRoleObjects_NoAlwaysIncludeSupervisor confirms the generated
// role carries no supervisor safety net, unlike hand-authored roles such as
// appworld_agent (AlwaysInclude: orchestrator_submit_decision + friends).
// Without one, a generated role that runs out of turns cannot even ask for
// a decision.
func TestGenerateRoleObjects_NoAlwaysIncludeSupervisor(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"supervisor": {
				ID:      "supervisor",
				Command: "python",
				AvailableTools: []string{
					"orchestrator_submit_decision",
					"orchestrator_complete_task",
				},
			},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	bindings := reg.ResolveToolBindings(newGeneratedRoleShape(), nil)
	for _, b := range bindings {
		if len(b.AlwaysInclude) > 0 {
			t.Fatalf("unexpected AlwaysInclude on generated role: %+v", b)
		}
	}
}
