package core

import (
	"fmt"

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

	if len(additionalMCPs) > 0 && !role.AllowDynamicMCPs {
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
		extraBindings = append(extraBindings, config.MCPBinding{
			MCPID:        mcpID,
			AllowedTools: allowed,
		})
	}

	// Use registry helper to resolve final set
	return hub.Registry.ResolveToolBindings(role, extraBindings), nil
}

// Note: added a small helper to handle nil map in ResolveFinalMCPs
func handleAllowlists(allowlists map[string][]string, mcpID string) []string {
	if allowlists == nil {
		return nil
	}
	return allowlists[mcpID]
}
