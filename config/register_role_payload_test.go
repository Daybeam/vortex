package config

import (
	"encoding/json"
	"testing"
)

// ─── Why this file exists ─────────────────────────────────────────────────
//
// docs 52-planner-wiring-design.md §11.6 tells operators they can create a
// role via orchestrator_invoke(subsystem="config", action="register_role"),
// and that arbitrary Role fields are accepted because the handler marshals
// the whole args map and hands it to CommitOps -> ApplyPatch.
//
// That is only half the story, and the missing half is a silent failure:
// ApplyPatch json.Unmarshals into the concrete Role type, so:
//
//   - A field name that does not resolve to a Role struct field / json tag
//     is DROPPED without an error (encoding/json default).
//   - always_include is an MCPBinding field, NOT a Role field, so putting
//     it at the top level of a role payload silently does nothing.
//
// These tests pin both behaviors so the operator-facing doc stays honest,
// and so a future strict-decoding change (DisallowUnknownFields) surfaces
// here first.

// TestApplyPatch_RegisterRoleAcceptsUndocumentedFields proves the documented
// escape hatch works: instruction / rules / bound_mcp_bindings are not in the
// register_role Parameters list but DO land on the Role.
func TestApplyPatch_RegisterRoleAcceptsUndocumentedFields(t *testing.T) {
	reg := &Registry{Roles: map[string]*Role{}}

	payload := `{
		"id": "draft_analyst",
		"name": "Draft Analyst",
		"base_capability": "analysis",
		"instruction": "Analyze carefully.",
		"rules": "Never write outside the output dir.",
		"bound_skills": [],
		"allow_dynamic_skills": false,
		"allow_dynamic_mcps": false,
		"bound_mcp_bindings": [
			{"mcp_id": "appworld", "allowed_tools": ["venmo__login"],
			 "always_include": ["orchestrator_submit_decision"]}
		]
	}`

	op := PatchOp{Op: OpAdd, Path: "/roles/draft_analyst", Value: json.RawMessage(payload)}
	if err := ApplyPatch(reg, []PatchOp{op}); err != nil {
		t.Fatalf("ApplyPatch failed: %v", err)
	}

	r, ok := reg.Roles["draft_analyst"]
	if !ok || r == nil {
		t.Fatal("role was not registered")
	}
	if r.Instruction != "Analyze carefully." {
		t.Errorf("instruction not persisted: %q", r.Instruction)
	}
	if r.Rules != "Never write outside the output dir." {
		t.Errorf("rules not persisted: %q", r.Rules)
	}
	if len(r.BoundMCPBindings) != 1 {
		t.Fatalf("expected 1 binding, got %d: %+v", len(r.BoundMCPBindings), r.BoundMCPBindings)
	}
	b := r.BoundMCPBindings[0]
	if b.MCPID != "appworld" {
		t.Errorf("mcp_id = %q, want appworld", b.MCPID)
	}
	// always_include lives on MCPBinding — the NESTED form must survive.
	if len(b.AlwaysInclude) != 1 || b.AlwaysInclude[0] != "orchestrator_submit_decision" {
		t.Errorf("nested always_include not persisted: %+v", b.AlwaysInclude)
	}
}

// TestApplyPatch_RoleTopLevelAlwaysIncludeIsSilentlyDropped locks the trap:
// always_include at the ROLE level (not inside bound_mcp_bindings) parses
// without error and vanishes. Operators must nest it.
func TestApplyPatch_RoleTopLevelAlwaysIncludeIsSilentlyDropped(t *testing.T) {
	reg := &Registry{Roles: map[string]*Role{}}

	// always_include placed at the top level instead of inside the binding.
	payload := `{
		"id": "wrong_placement",
		"name": "Wrong Placement",
		"base_capability": "analysis",
		"always_include": ["orchestrator_submit_decision"],
		"bound_skills": [],
		"allow_dynamic_skills": false,
		"allow_dynamic_mcps": false,
		"bound_mcp_bindings": [
			{"mcp_id": "appworld", "allowed_tools": ["venmo__login"]}
		]
	}`

	op := PatchOp{Op: OpAdd, Path: "/roles/wrong_placement", Value: json.RawMessage(payload)}
	if err := ApplyPatch(reg, []PatchOp{op}); err != nil {
		t.Fatalf("ApplyPatch should NOT error on an unknown field — that is the trap; got: %v", err)
	}

	r := reg.Roles["wrong_placement"]
	if r == nil {
		t.Fatal("role was not registered")
	}
	// The role exists and looks fine, but carries no safety net.
	if len(r.BoundMCPBindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(r.BoundMCPBindings))
	}
	if got := r.BoundMCPBindings[0].AlwaysInclude; len(got) != 0 {
		t.Fatalf("unexpected AlwaysInclude: %+v", got)
	}
}

// TestApplyPatch_UnknownRoleFieldIsSilentlyDropped documents the general rule
// behind the trap above: any non-existent field name is dropped, not rejected.
func TestApplyPatch_UnknownRoleFieldIsSilentlyDropped(t *testing.T) {
	reg := &Registry{Roles: map[string]*Role{}}

	payload := `{"id":"ghost_field","name":"Ghost","base_capability":"analysis",
		"this_field_does_not_exist": "whatever","bound_skills":[],
		"allow_dynamic_skills":false,"allow_dynamic_mcps":false,
		"bound_mcp_bindings":[]}`

	op := PatchOp{Op: OpAdd, Path: "/roles/ghost_field", Value: json.RawMessage(payload)}
	if err := ApplyPatch(reg, []PatchOp{op}); err != nil {
		t.Fatalf("unknown fields are silently ignored today; if strict decoding was "+
			"introduced this test should be updated to expect an error. got: %v", err)
	}
	if reg.Roles["ghost_field"] == nil {
		t.Fatal("role should still be created despite the unknown field")
	}
}
