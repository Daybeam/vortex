package config

import "testing"

// TestBuiltinMetaRolesAreAdminOnlyByDesign pins the answer to "can a public
// (non-admin) caller use the builtin roles at all?".
//
// It is YES-THREE-TIMES-NO, and that is intentional (AUTH-1, 2026-10-23): the
// three builtin roles are capability/plan meta surfaces —
//
//	sop_generator     creates roles, skills and MCP mounts (persistent)
//	decision_arbiter  picks outcomes for blocked decisions
//	task_planner      rewrites the DAG
//
// — so each declares CallableTiers: ["admin"]. A public caller naming one of
// them is refused at SUBMIT time with
// `role %q is not callable by a non-admin submitter`, which is a DIFFERENT
// refusal from the mount gate's
// `role %q is not available to non-admin submitters`. Confusing the two sent a
// whole 2026-10-26 investigation down the wrong path; keep them distinct.
//
// Note what this test does NOT say: it says nothing about user-defined roles.
// Those default to CallableTiers empty ⇒ callable by everyone, and are then
// bounded by the mount gate instead. That second gate is where "an MCP with no
// mount_policy defaults to admin_only" lives (config/mount_policy.go).
func TestBuiltinMetaRolesAreAdminOnlyByDesign(t *testing.T) {
	want := map[string]bool{
		"sop_generator":    true,
		"decision_arbiter": true,
		"task_planner":     true,
	}
	seen := map[string]bool{}
	for i := range builtinRoles {
		r := &builtinRoles[i]
		adminOnly, checked := want[r.ID]
		if !checked {
			continue
		}
		seen[r.ID] = true
		if r.CallableByAdmin(false) && adminOnly {
			t.Errorf("%q is callable by a non-admin submitter — AUTH-1 says the "+
				"meta roles must be admin-only; if this is deliberate, change the "+
				"expectation here AND the doc comment in core/scheduler_submit_gate.go",
				r.ID)
		}
		if !r.CallableByAdmin(true) {
			t.Errorf("%q must stay callable by admin", r.ID)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("builtin role %q disappeared from builtinRoles — update this "+
				"test if it was renamed, do not delete the case", id)
		}
	}
}

// TestMCPMountPolicyDefaultsToAdminOnly pins the default that made every
// unclassified MCP look privileged. If this ever flips to open, that is a
// security change: every MCP that forgets the field becomes reachable by public
// callers.
func TestMCPMountPolicyDefaultsToAdminOnly(t *testing.T) {
	for _, pol := range []string{"", "unknown-garbage"} {
		m := &MCPDef{ID: "x", MountPolicy: pol}
		if got := m.EffectiveMountPolicy(); got != MountPolicyAdminOnly {
			t.Fatalf("MCP mount_policy %q resolved to %q; the safe default is "+
				"admin_only — flipping it exposes every unclassified MCP to public "+
				"callers", pol, got)
		}
	}
	if got := (&MCPDef{ID: "x", MountPolicy: MountPolicyOpen}).EffectiveMountPolicy(); got != MountPolicyOpen {
		t.Fatalf("explicit open must be honoured, got %q", got)
	}
	// Skills are the opposite default on purpose: prompt-only surface.
	if got := (&Skill{ID: "s", MountPolicy: ""}).EffectiveMountPolicy(); got != MountPolicyOpen {
		t.Fatalf("skill mount_policy default must stay open, got %q", got)
	}
}
