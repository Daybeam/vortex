package config

// Mount-policy values for MCPDef.MountPolicy, checked by ResolveFinalMCPs for
// dynamic (unbound) mounts. See
// docs/plan/MCP_MOUNT_POLICY_AND_TIER_PROPAGATION_DESIGN.md.
const (
	// MountPolicyOpen: any submitter tier may mount the MCP dynamically.
	MountPolicyOpen = "open"
	// MountPolicyApproval: dynamic mount requires human approval for non-admin
	// submitters (human_approval_required). Admin mounts without approval.
	MountPolicyApproval = "approval"
	// MountPolicyAdminOnly: dynamic mount requires admin tier (default).
	MountPolicyAdminOnly = "admin_only"
)

// EffectiveMountPolicy returns the policy to apply for a dynamic mount,
// defaulting to MountPolicyAdminOnly for empty/unknown values.
func (m *MCPDef) EffectiveMountPolicy() string {
	if m == nil || m.MountPolicy == "" {
		return MountPolicyAdminOnly
	}
	switch m.MountPolicy {
	case MountPolicyOpen, MountPolicyApproval, MountPolicyAdminOnly:
		return m.MountPolicy
	default:
		return MountPolicyAdminOnly
	}
}
