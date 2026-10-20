package core

import (
	"fmt"
	"log/slog"

	"github.com/daybeam/vortex/config"
)

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
func ResolveFinalSkills(hub *ContextHub, roleID string, additionalSkills []string) ([]string, error) {
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
func ResolveFinalMCPs(hub *ContextHub, roleID string, additionalMCPs []string, additionalToolAllowlists map[string][]string) ([]config.MCPBinding, error) {
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
	dynamicCount := 0
	for _, mcpID := range additionalMCPs {
		if !boundMCP[mcpID] {
			dynamicCount++
		}
	}
	if dynamicCount > 0 && !role.AllowDynamicMCPs {
		return nil, fmt.Errorf("role %q does not allow dynamic MCPs", roleID)
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
