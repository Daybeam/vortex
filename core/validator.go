package core

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/daybeam/vortex/config"
)

// ErrMountApprovalRequired signals that an MCP mount is gated behind human
// approval for a non-admin submitter (MCP mount_policy: "approval"). Kept as
// the sentinel for errors.Is; ResolveFinalMCPs returns the richer
// *MountApprovalRequiredError so the caller can name the affected MCPs.
var ErrMountApprovalRequired = errors.New("dynamic MCP mount requires human approval")

// MountApprovalRequiredError is returned by ResolveFinalMCPs/ResolveFinalSkills
// when one or more mounts (MCPs or skills) are gated behind mount_policy=approval
// for a non-admin submitter. It carries the affected ids so the scheduler can
// raise a human_approval_required decision naming them (G2; skills added D3,
// 2026-10-22). errors.Is(err, ErrMountApprovalRequired) still holds.
type MountApprovalRequiredError struct {
	RoleID   string
	MCPIDs   []string
	SkillIDs []string
}

func (e *MountApprovalRequiredError) Error() string {
	return fmt.Sprintf("%s: role %q, mcps %v, skills %v", ErrMountApprovalRequired, e.RoleID, e.MCPIDs, e.SkillIDs)
}

// Is lets errors.Is(err, ErrMountApprovalRequired) match this richer error.
func (e *MountApprovalRequiredError) Is(target error) bool {
	return target == ErrMountApprovalRequired
}

// ValidationResult carries the outcome of a skill/mcp combination check.
type ValidationResult struct {
	Valid  bool
	Reason string
	Tokens int
	Skills []string
	MCPs   []config.MCPBinding
}

// ValidateCombination checks if a set of skill IDs can be used together without conflict.
func ValidateCombination(hub *ContextHub, skillIDs []string) (bool, string) {
	for _, sid := range skillIDs {
		skill := hub.GetSkill(sid)
		if skill == nil {
			return false, fmt.Sprintf("skill %q not found in registry or session", sid)
		}

		// Check for incompatible skills
		for _, inc := range skill.Incompatible {
			for _, other := range skillIDs {
				if inc == other {
					return false, fmt.Sprintf("skill %q is incompatible with %q", sid, other)
				}
			}
		}

		// Check for missing dependencies
		for _, req := range skill.Requires {
			found := false
			for _, other := range skillIDs {
				if req == other {
					found = true
					break
				}
			}
			if !found {
				return false, fmt.Sprintf("skill %q requires %q to also be included", sid, req)
			}
		}
	}
	return true, ""
}

// ResolveFinalSkills merges a role's bound skills with dynamic additions and validates the result.
func ResolveFinalSkills(hub *ContextHub, roleID string, additionalSkills []string, submitterIsAdmin bool) ([]string, error) {
	role := hub.GetRole(roleID)
	if role == nil {
		return nil, fmt.Errorf("role %q not found", roleID)
	}

	if len(additionalSkills) > 0 && !role.AllowDynamicSkills {
		return nil, fmt.Errorf("role %q does not allow dynamic skills", roleID)
	}

	if len(additionalSkills) > role.MaxAdditionalSkills {
		return nil, fmt.Errorf("requested %d additional skills but role %q allows max %d", len(additionalSkills), roleID, role.MaxAdditionalSkills)
	}

	// SECURITY (D3, 2026-10-22): skill granting mirrors the MCP mount gate —
	// Skill.MountPolicy applies to BOUND and dynamic grants for non-admin
	// submitters. Skills default to open (prompt-level surface, no executable
	// capability); this gates explicitly marked sensitive skills. `approval`
	// collects into a human_approval_required decision (G2 chain); admin_only
	// is a hard deny.
	if !submitterIsAdmin {
		var approvalNeeded []string
		check := func(sid string) error {
			def := hub.GetSkill(sid)
			if def == nil {
				return nil // capability text / unknown id — resolved later
			}
			switch def.EffectiveMountPolicy() {
			case config.MountPolicyOpen:
				return nil
			case config.MountPolicyApproval:
				if hub.Graph == nil || !hub.Graph.IsSkillMountApproved(sid) {
					approvalNeeded = append(approvalNeeded, sid)
				}
				return nil
			default:
				return fmt.Errorf("skill %q is admin_only; non-admin submitters may not be granted it (role %q)", sid, roleID)
			}
		}
		for _, sid := range role.BoundSkills {
			if err := check(sid); err != nil {
				return nil, err
			}
		}
		for _, sid := range additionalSkills {
			if err := check(sid); err != nil {
				return nil, err
			}
		}
		if len(approvalNeeded) > 0 {
			return nil, &MountApprovalRequiredError{RoleID: roleID, SkillIDs: approvalNeeded}
		}
	}

	// Merge bound and additional
	final := make([]string, 0, len(role.BoundSkills)+len(additionalSkills))
	final = append(final, role.BoundSkills...)

	seen := make(map[string]bool)
	for _, s := range role.BoundSkills {
		seen[s] = true
	}
	for _, s := range additionalSkills {
		if !seen[s] {
			final = append(final, s)
			seen[s] = true
		}
	}

	// Filter out non-existent skills if the role is flexible
	if role.AllowDynamicSkills {
		filtered := make([]string, 0, len(final))
		for _, sid := range final {
			if hub.GetSkill(sid) != nil {
				filtered = append(filtered, sid)
			} else if matched := hub.FindSkillByCapability(sid); matched != nil {
				// FIX (2026-08-06): Support natural language capability matching.
				// If sid is "fetch market data", it will match skill_fetch_market_data.
				filtered = append(filtered, matched.ID)
			} else {
				// Log a warning or signal field deposit for observability
				fmt.Printf("[WARN] Role %q: Skill %q not found via ID or Capability matching, skipping.\n", roleID, sid)
			}
		}
		final = filtered
	}

	valid, reason := ValidateCombination(hub, final)
	if !valid {
		return nil, fmt.Errorf("invalid skill combination: %s", reason)
	}

	return final, nil
}

// ResolveFinalMCPs merges a role's bound MCP bindings with dynamic additions.
// submitterIsAdmin controls whether dynamic MCP mounting is permitted at all —
// the primary check is in tools_task.go (entry point), this is defense-in-depth.
// SECURITY FIX (2026-10-21): previously, only role.AllowDynamicMCPs was checked,
// which let public callers use roles like sop_generator (AllowDynamicMCPs=true)
// to mount arbitrary MCPs via additional_mcps.
// resolveMountPolicyGate enforces the submitter × mount-policy gate for a
// role's bound MCPs and the caller's additional MCPs. It returns the MCP ids
// that need human approval (policy=approval, non-admin, not yet approved) plus
// a hard error for admin_only mounts. Extracted from ResolveFinalMCPs to keep
// that function within the size ratchet (G2, 2026-10-22).
//
// SECURITY (2026-10-22, audit #3): the policy applies to ALREADY-BOUND MCPs
// too — otherwise naming any role_id hands a non-admin caller that role's
// privileged tools (e.g. a role bound to an admin_only MCP that can affect the
// local machine). MCPs safe for lower tiers must be marked mount_policy=open.
// Re-specifying a bound MCP via additional_mcps is still not a NEW mount (the
// G1/G11 fix) — it is covered by the bound check rather than the dynamic check.
//
// SECURITY FIX (2026-10-21): the gate is driven by the MCP's MountPolicy
// (open / approval / admin_only), not by the role flag AllowDynamicMCPs.
func resolveMountPolicyGate(hub *ContextHub, role *config.Role, roleID string, additionalMCPs []string, boundMCP map[string]bool, submitterIsAdmin bool) ([]string, error) {
	// A prior human_approval_required decision may have recorded this MCP as
	// approved for the task (G2) — then the approval gate is satisfied.
	approved := func(mcpID string) bool {
		return hub != nil && hub.Graph != nil && hub.Graph.IsMCPMountApproved(mcpID)
	}
	var approvalNeeded []string

	if !submitterIsAdmin {
		for _, b := range role.BoundMCPBindings {
			if b.MCPID == "" {
				continue
			}
			policy := config.MountPolicyAdminOnly
			if def := hub.GetMCP(b.MCPID); def != nil {
				policy = def.EffectiveMountPolicy()
			}
			switch policy {
			case config.MountPolicyOpen:
				// safe for any submitter tier
			case config.MountPolicyApproval:
				if !approved(b.MCPID) {
					approvalNeeded = append(approvalNeeded, b.MCPID)
				}
			default: // admin_only (empty/unknown)
				return nil, fmt.Errorf("role %q binds admin_only MCP %q; non-admin submitters may not use this role", roleID, b.MCPID)
			}
		}
	}

	for _, mcpID := range additionalMCPs {
		if boundMCP[mcpID] {
			continue // already covered by the bound check above (G1/G11 fix)
		}
		policy := config.MountPolicyAdminOnly
		if def := hub.GetMCP(mcpID); def != nil {
			policy = def.EffectiveMountPolicy()
		}
		switch policy {
		case config.MountPolicyOpen:
			// open: any submitter tier may mount dynamically.
		case config.MountPolicyApproval:
			if !submitterIsAdmin && !approved(mcpID) {
				approvalNeeded = append(approvalNeeded, mcpID)
			}
		default: // admin_only (empty/unknown)
			if !submitterIsAdmin {
				return nil, fmt.Errorf("dynamic MCP mounting requires admin tier (role %q, mcp %q)", roleID, mcpID)
			}
		}
	}
	return approvalNeeded, nil
}

func ResolveFinalMCPs(hub *ContextHub, roleID string, additionalMCPs []string, additionalToolAllowlists map[string][]string, submitterIsAdmin bool) ([]config.MCPBinding, error) {
	role := hub.GetRole(roleID)
	if role == nil {
		return nil, fmt.Errorf("role %q not found", roleID)
	}

	// An MCP the role ALREADY binds is not a dynamic addition.
	//
	// The planner names an mcp_id on each step only to say "this step uses
	// appworld" — which is already the role's own surface. Counting that as a
	// dynamic mount made every planner-produced step fail instantly with
	// `role "appworld_agent" does not allow dynamic MCPs` (observed 2026-10-19,
	// three steps dead in 1.6ms each, zero provider calls).
	//
	// Worse, it made a real fix unreachable: config.Registry.ResolveToolBindings
	// was taught on 2026-10-18 to let an extra binding NARROW a role-bound MCP
	// (see its "Extra bindings may NARROW" comment, which cites exactly this
	// role and this flag). That code never ran, because this guard returned
	// first. Only a genuinely new MCP should require the permission.
	boundMCP := make(map[string]bool, len(role.BoundMCPBindings))
	for _, b := range role.BoundMCPBindings {
		if b.MCPID != "" {
			boundMCP[b.MCPID] = true
		}
	}

	// SECURITY (2026-10-22, audit #3): the mount policy also applies to
	// ALREADY-BOUND MCPs — naming any role_id must not hand a non-admin caller
	// that role's privileged MCPs. An `approval` mount is collected for a
	// human_approval_required decision (G2); an admin_only mount is a hard deny.
	approvalNeeded, gateErr := resolveMountPolicyGate(hub, role, roleID, additionalMCPs, boundMCP, submitterIsAdmin)
	if gateErr != nil {
		return nil, gateErr
	}
	if len(approvalNeeded) > 0 {
		return nil, &MountApprovalRequiredError{RoleID: roleID, MCPIDs: approvalNeeded}
	}

	// Validate that all additional MCPs are known
	extraBindings := make([]config.MCPBinding, 0, len(additionalMCPs))
	for _, mcpID := range additionalMCPs {
		if hub.GetMCP(mcpID) == nil {
			// Defect 5 fix: try dynamic MCP registration via callback
			if hub.OnUnknownMCP != nil {
				if def := hub.OnUnknownMCP(mcpID); def != nil {
					hub.RegisterDynamicMCP(def)
				}
			}
			if hub.GetMCP(mcpID) == nil {
				return nil, fmt.Errorf("unknown MCP id %q (not in whitelist)", mcpID)
			}
		}

		allowed := []string{}
		if additionalToolAllowlists != nil {
			if tools, ok := additionalToolAllowlists[mcpID]; ok {
				allowed = tools
			}
		}
		// T7 fix (2026-10-18): intersect planner-specified allowlist with
		// discovered AvailableTools. On a cold engine, ParsePlanDAG accepts
		// tools as "unverified" (catalog was empty). At execution time, the
		// MCP handshake has run and AvailableTools is populated. This
		// intersection filters out any invented tool names. If the
		// intersection is empty (none of the planned tools exist), fall
		// back to full access (empty = unrestricted) rather than blocking
		// the step entirely.
		if mcpDef := hub.GetMCP(mcpID); mcpDef != nil && len(mcpDef.AvailableTools) > 0 && len(allowed) > 0 {
			discovered := make(map[string]bool, len(mcpDef.AvailableTools))
			for _, t := range mcpDef.AvailableTools {
				discovered[t] = true
			}
			intersected := make([]string, 0, len(allowed))
			for _, t := range allowed {
				if discovered[t] {
					intersected = append(intersected, t)
				}
			}
			if len(intersected) > 0 {
				allowed = intersected
			} else {
				// Intersection empty: fall back to full access.
				// T7 observability: warn so operators can detect silent
				// over-permissioning (planner specified tools, none exist).
				slog.Warn("tool allowlist intersection empty — falling back to full access",
					"mcp_id", mcpID,
					"role", roleID,
					"planned_tools", allowed,
					"discovered_count", len(mcpDef.AvailableTools),
				)
				allowed = []string{}
			}
		}
		extraBindings = append(extraBindings, config.MCPBinding{
			MCPID:        mcpID,
			AllowedTools: allowed,
		})
	}

	// Use registry helper to resolve final set
	return hub.Registry.ResolveToolBindings(role, extraBindings), nil
}
