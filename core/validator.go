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

// MountPolicyRejectedError is returned when a role is unusable at the caller's
// tier because one of its BOUND MCPs is admin_only and not approved.
//
// The message is deliberately vague (admin-hiding: the caller must not learn
// which MCP is privileged), but the fields are populated so the engine can log
// the real reason for the operator. Do not "improve" the message by adding the
// MCP id — that is a public-tier information leak; extend the log instead.
//
// ADDED (2026-10-26): the previous bare fmt.Errorf made two very different
// situations indistinguishable — "this role is genuinely privileged" and "this
// MCP simply never declared a mount_policy" (the default is admin_only). A
// whole eval round was lost to the second one.
type MountPolicyRejectedError struct {
	RoleID string
	MCPID  string
	Policy string // effective policy: admin_only when the field is empty/unknown
}

func (e *MountPolicyRejectedError) Error() string {
	// Intentionally no MCP id — see the type comment.
	return fmt.Sprintf("role %q is not available to non-admin submitters", e.RoleID)
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

// callerSuppliedMCPs returns the subset of the given MCP ids that were
// REGISTERED AT RUNTIME rather than published by the operator in config.json.
//
// Why this is the right dividing line for multi-tenancy (2026-10-26): the
// operator's config is the only place an admin can make a deliberate "this is
// fine for the public tier" decision, and MCPDef.MountPolicy is the only field
// that carries it. Everything in registry.DynamicMCPs arrived AFTER boot — a
// JIT tool the caller just created, a definition resolved through
// hub.OnUnknownMCP, a capability the driver discovered — so for those there is
// no operator decision to fall back on, and "no explicit policy" does not mean
// "withheld by an admin"; it means "created a second ago by the caller
// themselves". Treating that as admin_only is what made a public tenant unable
// to close a loop of its own.
//
// Ownership decides WHOSE risk it is; ServerRiskyMCP decides whether it is
// SERVER risk at all — and that check still wins (see tenantSelfServiceOK).
//
// KNOWN GAP (tenancy, not reachability): DynamicMCPs is one process-global map,
// so tenant A's runtime-registered MCP is visible to tenant B by id. That
// cannot escalate — a server-risky definition is refused regardless of who
// registered it — but it is not isolation either. A real fix needs an owner id
// on MCPDef; do NOT paper over it by forcing DynamicMCPs back to admin_only,
// that would re-break the tenant loop this function exists to allow.
func (h *ContextHub) callerSuppliedMCPs(ids []string) map[string]bool {
	if h == nil || h.Registry == nil {
		return nil
	}
	out := make(map[string]bool, len(ids))
	h.Registry.Mu.RLock()
	defer h.Registry.Mu.RUnlock()
	for _, id := range ids {
		// GetMCP gives DynamicMCPs precedence, so "present in DynamicMCPs" is
		// exactly the effective definition the spawn will use.
		if id != "" && h.Registry.DynamicMCPs[id] != nil {
			out[id] = true
		}
	}
	return out
}

// tenantSelfServiceOK reports whether a non-admin caller may use mcpID without
// an operator decision: they supplied the definition themselves AND it cannot
// reach the host.
//
// Fails CLOSED on every unknown: an id that is not caller-supplied, a nil hub,
// a nil definition, and anything config.ServerRiskyMCP cannot vouch for.
func tenantSelfServiceOK(hub *ContextHub, mcpID string, callerSupplied map[string]bool) bool {
	if !callerSupplied[mcpID] {
		return false
	}
	if hub == nil || hub.Registry == nil {
		return false // no registry ⇒ cannot inspect the definition ⇒ refuse
	}
	return !config.ServerRiskyMCP(hub.GetMCP(mcpID))
}

// prepareCallerSuppliedMCPs resolves definitions the registry has never seen
// (hub.OnUnknownMCP) and then reports which of this spawn's MCP ids are
// runtime-owned.
//
// It MUST run before resolveMountPolicyGate. The pre-2026-10-26 order ran
// OnUnknownMCP after the gate, which meant an injected definition was always
// gated against GetMCP == nil ⇒ EffectiveMountPolicy() == admin_only ⇒ a
// non-admin caller could never mount the very MCP it had just provided.
func prepareCallerSuppliedMCPs(hub *ContextHub, boundIDs, additionalMCPs []string) map[string]bool {
	if hub == nil || hub.Registry == nil {
		return nil
	}
	for _, id := range additionalMCPs {
		if id == "" || hub.GetMCP(id) != nil || hub.OnUnknownMCP == nil {
			continue // already known, or the binding loop reports "unknown MCP id"
		}
		if def := hub.OnUnknownMCP(id); def != nil {
			hub.RegisterDynamicMCP(def)
		}
	}
	ids := make([]string, 0, len(boundIDs)+len(additionalMCPs))
	ids = append(ids, boundIDs...)
	ids = append(ids, additionalMCPs...)
	return hub.callerSuppliedMCPs(ids)
}

// dropServerRiskyAutoAttached removes auto-attached MCPs that a non-admin
// submitter must not inherit.
//
// Auto-attach is the only mount path that never sees resolveMountPolicyGate:
// Registry.ResolveToolBindings adds EVERY entry of DynamicMCPs whenever
// role.AllowDynamicMCPs is set. Without this filter a public caller naming any
// such role (sop_generator is one that is builtin-visible) inherited whatever
// an admin had registered at runtime — including an UNSANDBOXED JIT tool,
// since ServerRiskyMCP is true for Command != "" && !Sandboxed. That is a
// direct "public caller runs code with the engine's privileges" hole.
//
// Only ids the caller did NOT name and the role does NOT bind are eligible, so
// an operator's explicit mount_policy=open decision on a bound or named MCP is
// still honoured.
func dropServerRiskyAutoAttached(hub *ContextHub, bindings []config.MCPBinding, boundMCP map[string]bool, additionalMCPs []string) []config.MCPBinding {
	if hub == nil || hub.Registry == nil {
		// Unreachable from ResolveFinalMCPs (it dereferences both first), but if
		// it ever happens, return NOTHING rather than everything: a step with no
		// tools fails visibly, an inherited unsandboxed process does not
		// (no silent fallbacks).
		slog.Error("cannot inspect MCP definitions while filtering auto-attach — dropping the whole binding set")
		return nil
	}
	named := make(map[string]bool, len(additionalMCPs))
	for _, id := range additionalMCPs {
		named[id] = true
	}
	kept := bindings[:0]
	for _, b := range bindings {
		def := hub.GetMCP(b.MCPID)
		// An explicit mount_policy=open is an operator DECLARATION and wins over
		// the default inference — same rule as the bound/named paths, so the
		// three mount routes cannot disagree about what "open" means.
		if !boundMCP[b.MCPID] && !named[b.MCPID] &&
			config.ServerRiskyMCP(def) && def.EffectiveMountPolicy() != config.MountPolicyOpen {
			slog.Warn("dropping auto-attached server-risky MCP for a non-admin submitter",
				"mcp_id", b.MCPID,
				"why", "allow_dynamic_mcps auto-attach bypasses the mount-policy gate",
				"fix", "bind it explicitly with mount_policy=open if it is really intended for every tenant",
			)
			continue
		}
		kept = append(kept, b)
	}
	return kept
}

func resolveMountPolicyGate(hub *ContextHub, role *config.Role, roleID string, additionalMCPs []string, boundMCP map[string]bool, submitterIsAdmin bool, callerSupplied map[string]bool) ([]string, error) {
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
				// Multi-tenant rule (2026-10-26): an MCP the CALLER brought
				// along — anything in registry.DynamicMCPs, i.e. their own JIT
				// tool or a definition they supplied through additional_mcps —
				// is tenant-owned, so "no explicit policy" must not hand it to
				// admin. Otherwise a public tenant can never complete a closed
				// loop without an operator editing the global config. Anything
				// that can reach the SERVER is still refused, because
				// tenantSelfServiceOK checks ServerRiskyMCP first.
				if tenantSelfServiceOK(hub, b.MCPID, callerSupplied) {
					continue
				}
				// SECURITY (2026-10-24, admin-hiding): do NOT echo the MCP id —
				// admin-only MCPs must be invisible to public-tier submitters, so
				// the rejection must not reveal which MCP is privileged.
				//
				// The TYPE carries the detail for the engine's own log; the
				// message does not (2026-10-26: a vague error cost a 5.5 h round).
				return nil, &MountPolicyRejectedError{RoleID: roleID, MCPID: b.MCPID, Policy: policy}
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
				// Same multi-tenant rule as the bound branch above: a definition
				// the caller registered at runtime is theirs to use, provided it
				// cannot reach the host. The error below keeps naming the MCP id
				// because the caller supplied that id themselves — nothing hidden.
				if tenantSelfServiceOK(hub, mcpID, callerSupplied) {
					continue
				}
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
	//
	// Order matters (multi-tenant rule, 2026-10-26): resolve caller-supplied
	// definitions into DynamicMCPs FIRST, otherwise the gate inspects nil and
	// defaults every one of them to admin_only.
	boundIDs := make([]string, 0, len(role.BoundMCPBindings))
	for _, b := range role.BoundMCPBindings {
		boundIDs = append(boundIDs, b.MCPID)
	}
	callerSupplied := prepareCallerSuppliedMCPs(hub, boundIDs, additionalMCPs)

	approvalNeeded, gateErr := resolveMountPolicyGate(hub, role, roleID, additionalMCPs, boundMCP, submitterIsAdmin, callerSupplied)
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
			} else if !submitterIsAdmin {
				// SECURITY (AUTH-4, 2026-10-23): for a non-admin submitter an
				// empty intersection must FAIL CLOSED. The old fallback set
				// allowed = [] which the "empty = unrestricted" convention reads
				// as FULL ACCESS — so a planner that named only non-existent
				// tools silently handed the caller the MCP's entire surface.
				// An admin keeps the permissive fallback (trusted tier).
				return nil, fmt.Errorf("role %q mcp %q: none of the requested tools %v exist on this server; refusing full access for a non-admin submitter", roleID, mcpID, allowed)
			} else {
				// Intersection empty: admin fallback to full access.
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
	final := hub.Registry.ResolveToolBindings(role, extraBindings)

	// AUTH-5 (2026-10-26): auto-attach is the one mount path that never passed
	// the gate, so a non-admin submitter must not inherit server-risky runtime
	// MCPs through it. See dropServerRiskyAutoAttached.
	if !submitterIsAdmin {
		final = dropServerRiskyAutoAttached(hub, final, boundMCP, additionalMCPs)
	}
	return final, nil
}
