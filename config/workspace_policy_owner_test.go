package config

import (
	"testing"
)

// O3 (audit register, 2026-11-04): WorkspacePolicyConfig.PerOwner was declared by
// TIER_GATED_WORKSPACE_SCOPE_DESIGN §3.6 and then ignored by GetWorkspacePolicy,
// which returned the global policy for every caller. These tests pin the merge,
// and above all that it can only narrow: core/scheduler_submit.go reads an EMPTY
// AllowedRoots as "no whitelist → allow everything", so a naive intersection that
// lands on empty would widen the scope instead of tightening it.

func newPolicyRegistry(policy WorkspacePolicyConfig) *Registry {
	return &Registry{System: SystemSettings{WorkspacePolicy: policy}}
}

func TestGetWorkspacePolicy_OwnerOverrideNarrowsWithinGlobalRoot(t *testing.T) {
	r := newPolicyRegistry(WorkspacePolicyConfig{
		Enabled:      true,
		AllowedRoots: []string{"workspace"},
		PerOwner: map[string]PerOwnerPolicy{
			"tenant-a": {AllowedRoots: []string{"workspace/tenant-a"}},
		},
	})

	got := r.GetWorkspacePolicy("tenant-a")
	if len(got.AllowedRoots) != 1 || got.AllowedRoots[0] != "workspace/tenant-a" {
		t.Fatalf("owner override was not applied: %+v", got.AllowedRoots)
	}
}

func TestGetWorkspacePolicy_EscapingOverrideIsDroppedNotWidened(t *testing.T) {
	r := newPolicyRegistry(WorkspacePolicyConfig{
		Enabled:      true,
		AllowedRoots: []string{"workspace"},
		PerOwner: map[string]PerOwnerPolicy{
			// Nothing here sits under the global root. Honouring it would let
			// tenant-b submit outside the whitelist; emptying the list instead
			// would be worse, because the consumer reads empty as "allow all".
			"tenant-b": {AllowedRoots: []string{"..", "C:\\Users\\other"}},
		},
	})

	got := r.GetWorkspacePolicy("tenant-b")
	if len(got.AllowedRoots) != 1 || got.AllowedRoots[0] != "workspace" {
		t.Fatalf("escaping override widened the scope: %+v", got.AllowedRoots)
	}
}

func TestGetWorkspacePolicy_OverrideOnEmptyGlobalListIsTheNarrowingCase(t *testing.T) {
	// Global list empty means "no whitelist configured" today, so an override here
	// strictly reduces what the owner may submit into. That is the multi-tenant
	// case the design actually wanted.
	r := newPolicyRegistry(WorkspacePolicyConfig{
		Enabled:      true,
		AllowedRoots: nil,
		PerOwner: map[string]PerOwnerPolicy{
			"tenant-c": {AllowedRoots: []string{"tenants/c"}},
		},
	})

	got := r.GetWorkspacePolicy("tenant-c")
	if len(got.AllowedRoots) != 1 || got.AllowedRoots[0] != "tenants/c" {
		t.Fatalf("override ignored when the global list is empty: %+v", got.AllowedRoots)
	}
}

func TestGetWorkspacePolicy_LocalAndUnknownOwnerKeepGlobalScope(t *testing.T) {
	policy := WorkspacePolicyConfig{
		Enabled:      true,
		AllowedRoots: []string{"workspace"},
		PerOwner: map[string]PerOwnerPolicy{
			"tenant-a": {AllowedRoots: []string{"workspace/tenant-a"}},
		},
	}
	r := newPolicyRegistry(policy)

	for _, owner := range []string{"", "tenant-unknown"} {
		got := r.GetWorkspacePolicy(owner)
		if len(got.AllowedRoots) != 1 || got.AllowedRoots[0] != "workspace" {
			t.Errorf("owner %q changed the global scope: %+v", owner, got.AllowedRoots)
		}
	}
}

func TestGetWorkspacePolicy_ReturnedCopyLeaksNothing(t *testing.T) {
	r := newPolicyRegistry(WorkspacePolicyConfig{
		Enabled:      true,
		AllowedRoots: []string{"workspace"},
		PerOwner: map[string]PerOwnerPolicy{
			"tenant-a": {AllowedRoots: []string{"workspace/tenant-a"}},
		},
	})

	got := r.GetWorkspacePolicy("tenant-a")
	if got.PerOwner != nil {
		t.Errorf("returned policy still carries every owner's overrides: %+v", got.PerOwner)
	}
	got.AllowedRoots[0] = "mutated"
	if r.System.WorkspacePolicy.AllowedRoots[0] != "workspace" {
		t.Errorf("caller mutation reached the registry: %+v", r.System.WorkspacePolicy.AllowedRoots)
	}
	if len(r.System.WorkspacePolicy.PerOwner["tenant-a"].AllowedRoots) != 1 {
		t.Errorf("registry's own override list was edited through the copy")
	}
}

func TestGetWorkspacePolicy_DisabledPolicyStillReturnsNil(t *testing.T) {
	r := newPolicyRegistry(WorkspacePolicyConfig{
		Enabled:      false,
		AllowedRoots: []string{"workspace"},
		PerOwner:     map[string]PerOwnerPolicy{"tenant-a": {AllowedRoots: []string{"x"}}},
	})
	if got := r.GetWorkspacePolicy("tenant-a"); got != nil {
		t.Errorf("disabled policy returned a scope: %+v", got)
	}
}
