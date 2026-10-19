package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── Why this file exists ─────────────────────────────────────────────────
//
// AppendSessionRole namespaces a generated role's id and keeps it out of the
// global registry. That property is load-bearing for the ephemeral path: it
// is what stops a generated role from shadowing a real one and what keeps it
// invisible to other tasks.
//
// A sibling concern — cloning another role's tool surface onto the generated
// role — briefly lived here and was REVERTED (2026-10-18, docs 52 §12.3):
// tool surfaces belong to StepInput.AdditionalMCPs /
// AdditionalToolAllowlists, not to roles. Its tests were removed with it.

func roleWithBindings(id string, mcps ...string) *config.Role {
	bs := make([]config.MCPBinding, 0, len(mcps))
	for _, m := range mcps {
		bs = append(bs, config.MCPBinding{
			MCPID:         m,
			AllowedTools:  []string{m + "__login"},
			AlwaysInclude: []string{"orchestrator_submit_decision"},
		})
	}
	return &config.Role{ID: id, Name: id, BoundMCPBindings: bs}
}

func newTestRegistry(roles ...*config.Role) *config.Registry {
	m := make(map[string]*config.Role, len(roles))
	for _, r := range roles {
		m[r.ID] = r
	}
	return &config.Registry{
		Roles:       m,
		MCPs:        map[string]*config.MCPDef{},
		DynamicMCPs: map[string]*config.MCPDef{},
	}
}

// TestAppendSessionRole_NamespacesAndStaysOutOfRegistry confirms the "safe
// temporary role" property: the id cannot shadow a real role and the global
// registry is untouched.
func TestAppendSessionRole_NamespacesAndStaysOutOfRegistry(t *testing.T) {
	reg := newTestRegistry(roleWithBindings("appworld_agent", "appworld"))
	graph := &schemas.TaskGraph{TaskID: "task_abc", SessionRoles: map[string]any{}}

	generated := &config.Role{ID: "appworld_agent"} // deliberately collides
	scoped, err := AppendSessionRole(graph, "task_abc", generated)
	if err != nil {
		t.Fatalf("AppendSessionRole failed: %v", err)
	}

	if scoped == "appworld_agent" {
		t.Fatal("session role id must be namespaced, never the raw id")
	}
	if _, ok := graph.GetSessionRole(scoped); !ok {
		t.Fatal("session role was not stored")
	}

	reg.Mu.RLock()
	_, leaked := reg.Roles[scoped]
	reg.Mu.RUnlock()
	if leaked {
		t.Error("ephemeral role must NOT appear in the global registry")
	}

	// The real role must be untouched.
	if reg.Roles["appworld_agent"].ID != "appworld_agent" {
		t.Error("global role was shadowed or mutated")
	}
}

// TestAppendSessionRole_RejectsNilInputs keeps the helper honest.
func TestAppendSessionRole_RejectsNilInputs(t *testing.T) {
	if _, err := AppendSessionRole(nil, "t", &config.Role{ID: "x"}); err == nil {
		t.Error("nil graph should be rejected")
	}
	if _, err := AppendSessionRole(&schemas.TaskGraph{}, "t", nil); err == nil {
		t.Error("nil role should be rejected")
	}
	if _, err := AppendSessionRole(&schemas.TaskGraph{}, "t", &config.Role{}); err == nil {
		t.Error("role without id should be rejected")
	}
}

// T2 regression: AppendSessionRole must force-clean dangerous flags on
// ephemeral roles before persisting, regardless of payload source.
// Prevents public-tier callers from injecting dynamic MCPs/skills or
// mutable roles via create_role.
func TestAppendSessionRole_ForceCleansDangerousFlags(t *testing.T) {
	graph := &schemas.TaskGraph{TaskID: "task_t2", SessionRoles: map[string]any{}}

	dangerous := &config.Role{
		ID:                "user_created",
		AllowDynamicMCPs:  true,
		AllowDynamicSkills: true,
		Generatable:       true,
		MutableByAgent:    true,
		BoundMCPBindings:  []config.MCPBinding{{MCPID: "evil_mcp", AllowedTools: []string{"*"}}},
	}

	scoped, err := AppendSessionRole(graph, "task_t2", dangerous)
	if err != nil {
		t.Fatalf("AppendSessionRole failed: %v", err)
	}

	stored, ok := graph.GetSessionRole(scoped)
	if !ok {
		t.Fatal("role not stored")
	}
	role, ok := stored.(*config.Role)
	if !ok {
		t.Fatalf("stored role is *config.Role, got %T", stored)
	}
	if role.AllowDynamicMCPs {
		t.Error("AllowDynamicMCPs must be false on ephemeral roles")
	}
	if role.AllowDynamicSkills {
		t.Error("AllowDynamicSkills must be false on ephemeral roles")
	}
	if role.Generatable {
		t.Error("Generatable must be false on ephemeral roles")
	}
	if role.MutableByAgent {
		t.Error("MutableByAgent must be false on ephemeral roles")
	}
	if len(role.BoundMCPBindings) != 0 {
		t.Errorf("BoundMCPBindings must be cleared, got %d bindings", len(role.BoundMCPBindings))
	}
}
