package config

// Mount-policy values, checked by ResolveFinalMCPs (MCPs) and
// ResolveFinalSkills (skills). See
// docs/completed/2026-10-22/MCP_MOUNT_POLICY_AND_TIER_PROPAGATION_DESIGN.md (MCPs) and
// docs/plan/MOUNT_POLICY_REMAINDER_DESIGN.md §2.R-b (skills, D3).
const (
	// MountPolicyOpen: any submitter tier may mount/use the resource.
	MountPolicyOpen = "open"
	// MountPolicyApproval: non-admin use requires a human_approval_required
	// decision (approve records it on the graph). Admin uses without approval.
	MountPolicyApproval = "approval"
	// MountPolicyAdminOnly: requires admin tier.
	MountPolicyAdminOnly = "admin_only"
)

// effectiveMountPolicy resolves a raw policy string against a fallback for
// empty/unknown values. MCPs fall back to admin_only (safe default); skills
// fall back to open (prompt-only surface — no executable capability).
func effectiveMountPolicy(policy, fallback string) string {
	switch policy {
	case MountPolicyOpen, MountPolicyApproval, MountPolicyAdminOnly:
		return policy
	default:
		return fallback
	}
}

// EffectiveMountPolicy returns the policy to apply for an MCP mount (bound or
// dynamic), defaulting to MountPolicyAdminOnly for empty/unknown values.
func (m *MCPDef) EffectiveMountPolicy() string {
	if m == nil {
		return MountPolicyAdminOnly
	}
	return effectiveMountPolicy(m.MountPolicy, MountPolicyAdminOnly)
}

// EffectiveMountPolicy returns the policy to apply when granting this skill
// (bound or dynamic). Skills default to open — their implementations are
// prompt-level (SkillImplementation.SystemPrompt) with no executable surface;
// real executables (MCPs, core tools) are gated separately. Mark a skill
// approval/admin_only to gate sensitive prompt content (e.g. credentials
// guidance) for non-admin submitters.
func (s *Skill) EffectiveMountPolicy() string {
	if s == nil {
		return MountPolicyOpen
	}
	return effectiveMountPolicy(s.MountPolicy, MountPolicyOpen)
}
