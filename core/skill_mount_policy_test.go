package core

import (
	"errors"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// skillPolicyHub builds a hub whose role binds skills of every mount policy
// (D3, 2026-10-22). Skills default to open (prompt-level surface), so the
// undeclared skill is usable by non-admin; the declared ones exercise the gate.
func skillPolicyHub(graph *schemas.TaskGraph) *ContextHub {
	hub := NewContextHub(&config.Registry{
		Roles: map[string]*config.Role{
			"worker": {
				ID: "worker", AllowDynamicSkills: true, MaxAdditionalSkills: 10,
				BoundSkills: []string{"open_sk", "default_sk"},
			},
			"approver": {
				ID: "approver", AllowDynamicSkills: true, MaxAdditionalSkills: 10,
				BoundSkills: []string{"approv_sk", "open_sk"},
			},
		},
		Skills: map[string]*config.Skill{
			"open_sk":    {ID: "open_sk", Capability: "cap_open", MountPolicy: config.MountPolicyOpen},
			"default_sk": {ID: "default_sk", Capability: "cap_default"}, // empty ⇒ open
			"approv_sk":  {ID: "approv_sk", Capability: "cap_approv", MountPolicy: config.MountPolicyApproval},
			"admin_sk":   {ID: "admin_sk", Capability: "cap_admin", MountPolicy: config.MountPolicyAdminOnly},
			"extra_open": {ID: "extra_open", Capability: "cap_extra_open"},
		},
	}, nil, nil)
	hub.Graph = graph
	return hub
}

// Regression (D3, 2026-10-22): skills default to open — a non-admin submitter
// keeps receiving prompt-level skills unless one is explicitly marked.
func TestResolveFinalSkills_DefaultOpenForNonAdmin(t *testing.T) {
	hub := skillPolicyHub(nil)
	final, err := ResolveFinalSkills(hub, "worker", []string{"extra_open"}, false)
	if err != nil {
		t.Fatalf("open/default skills must be grantable to non-admin, got: %v", err)
	}
	if len(final) != 3 {
		t.Fatalf("expected 3 skills (2 bound + 1 extra), got %v", final)
	}
}

// Regression (D3, 2026-10-22): an admin_only skill is hard-denied for a
// non-admin submitter, bound or dynamic; admin is unaffected.
func TestResolveFinalSkills_AdminOnlySkillDeniedForNonAdmin(t *testing.T) {
	// Dynamic grant of an admin_only skill is denied; admin is unaffected.
	hub2 := NewContextHub(&config.Registry{
		Roles: map[string]*config.Role{
			"dyn": {ID: "dyn", AllowDynamicSkills: true, MaxAdditionalSkills: 10},
		},
		Skills: map[string]*config.Skill{
			"admin_sk": {ID: "admin_sk", Capability: "cap_admin", MountPolicy: config.MountPolicyAdminOnly},
		},
	}, nil, nil)
	if _, err := ResolveFinalSkills(hub2, "dyn", []string{"admin_sk"}, false); err == nil {
		t.Fatal("dynamic admin_only skill must be denied for non-admin")
	}
	if _, err := ResolveFinalSkills(hub2, "dyn", []string{"admin_sk"}, true); err != nil {
		t.Fatalf("admin must be granted admin_only skills, got: %v", err)
	}
}

// Regression (D3, 2026-10-22): an `approval` skill short-circuits with the
// MountApprovalRequiredError carrying the skill ids (G2 chain, skills), and a
// recorded graph approval satisfies the gate on retry.
func TestResolveFinalSkills_ApprovalSkillDecisionFlow(t *testing.T) {
	graph := &schemas.TaskGraph{TaskID: "t1"}
	hub := skillPolicyHub(graph)

	_, err := ResolveFinalSkills(hub, "approver", nil, false)
	var ae *MountApprovalRequiredError
	if !errors.As(err, &ae) {
		t.Fatalf("approval skill must yield *MountApprovalRequiredError, got: %v", err)
	}
	if !errors.Is(err, ErrMountApprovalRequired) {
		t.Fatal("sentinel errors.Is must still hold for skill approvals")
	}
	if len(ae.SkillIDs) != 1 || ae.SkillIDs[0] != "approv_sk" {
		t.Fatalf("expected SkillIDs [approv_sk], got %v", ae.SkillIDs)
	}

	// Admin bypasses the approval gate.
	adminHub := skillPolicyHub(nil)
	if _, err := ResolveFinalSkills(adminHub, "approver", nil, true); err != nil {
		t.Fatalf("admin must be granted approval skills directly, got: %v", err)
	}

	// After the human approval is recorded, the non-admin retry passes.
	graph.ApproveSkillMounts(ae.SkillIDs, "", "")
	hub3 := NewContextHub(&config.Registry{
		Roles: map[string]*config.Role{
			"approver_role": {ID: "approver_role", BoundSkills: []string{"approv_sk", "open_sk"}},
		},
		Skills: hub.Registry.Skills,
	}, nil, nil)
	hub3.Graph = graph
	if _, err := ResolveFinalSkills(hub3, "approver_role", nil, false); err != nil {
		t.Fatalf("recorded approval must satisfy the skill gate on retry, got: %v", err)
	}
}

// Regression (D3, 2026-10-22): the approve path records skill approvals from a
// human_approval_required decision context (both []string and []any shapes).
func TestDecisionApprovalIDs_SkillContextShapes(t *testing.T) {
	mountsDec := &schemas.Decision{Context: map[string]any{
		"approval_required_mounts": []string{"mcp_a"},
		"approval_required_skills": []any{"sk_a", "sk_b"},
	}}
	if got := decisionApprovalMounts(mountsDec); len(got) != 1 || got[0] != "mcp_a" {
		t.Fatalf("mounts extraction failed: %v", got)
	}
	if got := decisionApprovalIDs(mountsDec, "approval_required_skills"); len(got) != 2 || got[0] != "sk_a" || got[1] != "sk_b" {
		t.Fatalf("skills extraction (post-JSON []any shape) failed: %v", got)
	}
}
