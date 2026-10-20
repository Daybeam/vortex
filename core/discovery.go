package core

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// discovery.go — Two-Layer Scoped Discovery backend.
//
// Implements docs/plan/TWO_LAYER_SCOPED_DISCOVERY_DESIGN.md §4.
// This is the reusable backend called by the MCP `discover` tool,
// the planner, and (in Phase 2) the spawner's lazy discovery path.
//
// Phase 1 (this file): scope enforcement + read from in-memory registry.
// No I/O, no LLM calls, no behavioral change to existing discovery.
// The `discover` MCP tool is additive — existing tools continue to work.

// ── Types ─────────────────────────────────────────────────────────────────

// DiscoveryScope defines the visibility boundary for a discover operation.
type DiscoveryScope struct {
	// Mode: "self" | "role" | "pool"
	Mode string
	// RoleID: the role whose bindings define the scope.
	//   Mode="self": the caller's role ID.
	//   Mode="role": the target role to inspect.
	//   Mode="pool": empty (ignored).
	RoleID string
	// AllowPool: true if the caller has admin permission for pool scope.
	// When false, Mode="pool" returns an error.
	AllowPool bool
}

// DiscoveryResult is the return type of DiscoverImpl.
type DiscoveryResult struct {
	Tools []schemas.ToolDefinition  `json:"tools,omitempty"`
	Roles []DiscoveryRoleSummary    `json:"roles,omitempty"`
	MCPs  []MCPSummary              `json:"mcps,omitempty"`
	Total int                      `json:"total"`
}

// DiscoveryRoleSummary is a lightweight role description for discovery results.
// Includes tool count so the planner can make informed assignments without
// a separate tools/list call per role.
// Distinct from capability_navigator.go's RoleSummary (which has Purpose/BestFor/Examples).
type DiscoveryRoleSummary struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	BaseCapability string `json:"base_capability"`
	ToolCount      int    `json:"tool_count"`
}

// MCPSummary is a lightweight MCP description for discovery results.
type MCPSummary struct {
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Command   string `json:"command,omitempty"`
	Trusted   bool   `json:"trusted"`
	Dynamic   bool   `json:"dynamic,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"` // RFC3339, dynamic MCPs only
}

// ── Scope Enforcement ─────────────────────────────────────────────────────

// enforceScope validates the scope and returns the set of MCP IDs visible
// within that scope. This is the permission gate — all discovery goes
// through here.
//
// Returns:
//   - mcpIDs: the set of MCP IDs the caller is allowed to see
//   - err: if scope is invalid or pool access is denied
func enforceScope(reg *config.Registry, scope DiscoveryScope) (mcpIDs []string, err error) {
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	switch scope.Mode {
	case "self":
		// Only the caller's role's BoundMCPBindings.
		if scope.RoleID == "" {
			return nil, fmt.Errorf("scope 'self' requires a role ID")
		}
		role, ok := reg.Roles[scope.RoleID]
		if !ok || role == nil {
			return nil, fmt.Errorf("role %q not found", scope.RoleID)
		}
		for _, b := range role.BoundMCPBindings {
			if b.MCPID != "" {
				mcpIDs = append(mcpIDs, b.MCPID)
			}
		}

	case "role":
		// Only the specified role's BoundMCPBindings.
		// Used by the planner to inspect what a target role can see.
		if scope.RoleID == "" {
			return nil, fmt.Errorf("scope 'role' requires a role ID")
		}
		role, ok := reg.Roles[scope.RoleID]
		if !ok || role == nil {
			return nil, fmt.Errorf("role %q not found", scope.RoleID)
		}
		for _, b := range role.BoundMCPBindings {
			if b.MCPID != "" {
				mcpIDs = append(mcpIDs, b.MCPID)
			}
		}

	case "pool":
		// All MCPs — requires admin permission.
		if !scope.AllowPool {
			return nil, fmt.Errorf("scope 'pool' requires admin permission")
		}
		for id := range reg.MCPs {
			mcpIDs = append(mcpIDs, id)
		}
		for id := range reg.DynamicMCPs {
			mcpIDs = append(mcpIDs, id)
		}

	default:
		return nil, fmt.Errorf("invalid scope mode %q (must be self, role, or pool)", scope.Mode)
	}

	return mcpIDs, nil
}

// ── Discovery Backend ─────────────────────────────────────────────────────

// DiscoverImpl is the single backend for all discovery operations.
// It is called by the MCP `discover` tool, the planner, and the spawner.
//
// Parameters:
//   - dtype: "tools" | "roles" | "mcps"
//   - query: optional keyword filter (case-insensitive substring match)
//   - page, pageSize: pagination (1-indexed)
//
// Phase 1: reads from in-memory registry only (no tools/list I/O).
// Phase 2 will add lazy tools/list for MCPs with empty FullToolDefinitions.
func DiscoverImpl(reg *config.Registry, scope DiscoveryScope, dtype, query string, page, pageSize int) (*DiscoveryResult, error) {
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if page <= 0 {
		page = 1
	}

	queryLower := strings.ToLower(query)

	switch dtype {
	case "tools":
		return discoverTools(reg, scope, queryLower, page, pageSize)
	case "roles":
		return discoverRoles(reg, scope, queryLower, page, pageSize)
	case "mcps":
		return discoverMCPs(reg, scope, queryLower, page, pageSize)
	default:
		return nil, fmt.Errorf("invalid type %q (must be tools, roles, or mcps)", dtype)
	}
}

// discoverTools returns tool definitions within the scope.
func discoverTools(reg *config.Registry, scope DiscoveryScope, queryLower string, page, pageSize int) (*DiscoveryResult, error) {
	mcpIDs, err := enforceScope(reg, scope)
	if err != nil {
		return nil, err
	}

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	idSet := make(map[string]bool, len(mcpIDs))
	for _, id := range mcpIDs {
		idSet[id] = true
	}

	var matched []schemas.ToolDefinition
	for id, mcp := range reg.MCPs {
		if !idSet[id] {
			continue
		}
		for _, tool := range mcp.FullToolDefinitions {
			if queryLower == "" ||
				strings.Contains(strings.ToLower(tool.Name), queryLower) ||
				strings.Contains(strings.ToLower(tool.Description), queryLower) {
				matched = append(matched, tool)
			}
		}
	}
	// Also check dynamic MCPs.
	for id, mcp := range reg.DynamicMCPs {
		if !idSet[id] {
			continue
		}
		for _, tool := range mcp.FullToolDefinitions {
			if queryLower == "" ||
				strings.Contains(strings.ToLower(tool.Name), queryLower) ||
				strings.Contains(strings.ToLower(tool.Description), queryLower) {
				matched = append(matched, tool)
			}
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Name < matched[j].Name
	})

	total := len(matched)
	start := (page - 1) * pageSize
	if start >= total {
		return &DiscoveryResult{Total: total}, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	return &DiscoveryResult{
		Tools: matched[start:end],
		Total: total,
	}, nil
}

// discoverRoles returns role summaries within the scope.
// For scope="self": returns only the caller's role.
// For scope="role": returns only the specified role.
// For scope="pool": returns all roles.
func discoverRoles(reg *config.Registry, scope DiscoveryScope, queryLower string, page, pageSize int) (*DiscoveryResult, error) {
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	var matched []DiscoveryRoleSummary

	switch scope.Mode {
	case "self", "role":
		role, ok := reg.Roles[scope.RoleID]
		if !ok || role == nil {
			return &DiscoveryResult{}, nil
		}
		matched = append(matched, roleToSummary(reg, role))

	case "pool":
		// All roles — requires admin permission (matches enforceScope pattern).
		// Regression: audit L-20-2 — discoverRoles was missing this check,
		// allowing public-tier callers to enumerate all roles.
		if !scope.AllowPool {
			return nil, fmt.Errorf("scope 'pool' requires admin permission")
		}
		for _, role := range reg.Roles {
			if role == nil {
				continue
			}
			if queryLower == "" ||
				strings.Contains(strings.ToLower(role.ID), queryLower) ||
				strings.Contains(strings.ToLower(role.Name), queryLower) ||
				strings.Contains(strings.ToLower(role.BaseCapability), queryLower) {
				matched = append(matched, roleToSummary(reg, role))
			}
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].ID < matched[j].ID
	})

	total := len(matched)
	start := (page - 1) * pageSize
	if start >= total {
		return &DiscoveryResult{Total: total}, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	return &DiscoveryResult{
		Roles: matched[start:end],
		Total: total,
	}, nil
}

// discoverMCPs returns MCP summaries within the scope.
func discoverMCPs(reg *config.Registry, scope DiscoveryScope, queryLower string, page, pageSize int) (*DiscoveryResult, error) {
	mcpIDs, err := enforceScope(reg, scope)
	if err != nil {
		return nil, err
	}

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	idSet := make(map[string]bool, len(mcpIDs))
	for _, id := range mcpIDs {
		idSet[id] = true
	}

	var matched []MCPSummary
	for id, mcp := range reg.MCPs {
		if !idSet[id] {
			continue
		}
		if queryLower == "" || strings.Contains(strings.ToLower(id), queryLower) {
			matched = append(matched, MCPSummary{
				ID:      id,
				URL:     mcp.URL,
				Command: mcp.Command,
				Trusted: mcp.Trusted,
			})
		}
	}
	for id, mcp := range reg.DynamicMCPs {
		if !idSet[id] {
			continue
		}
		if queryLower == "" || strings.Contains(strings.ToLower(id), queryLower) {
			matched = append(matched, MCPSummary{
				ID:        id,
				URL:       mcp.URL,
				Command:   mcp.Command,
				Trusted:   mcp.Trusted,
				Dynamic:   true,
				ExpiresAt: mcp.ExpiresAt.Format(time.RFC3339),
			})
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].ID < matched[j].ID
	})

	total := len(matched)
	start := (page - 1) * pageSize
	if start >= total {
		return &DiscoveryResult{Total: total}, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	return &DiscoveryResult{
		MCPs:  matched[start:end],
		Total: total,
	}, nil
}

// roleToSummary converts a Role to a DiscoveryRoleSummary with tool count.
func roleToSummary(reg *config.Registry, role *config.Role) DiscoveryRoleSummary {
	toolCount := 0
	for _, b := range role.BoundMCPBindings {
		if mcp, ok := reg.MCPs[b.MCPID]; ok && mcp != nil {
			toolCount += len(mcp.FullToolDefinitions)
		}
		if mcp, ok := reg.DynamicMCPs[b.MCPID]; ok && mcp != nil {
			toolCount += len(mcp.FullToolDefinitions)
		}
	}
	return DiscoveryRoleSummary{
		ID:             role.ID,
		Name:           role.Name,
		BaseCapability: role.BaseCapability,
		ToolCount:      toolCount,
	}
}

// ── Scope Parsing ─────────────────────────────────────────────────────────

// ParseScope parses a scope string like "self", "role:my_role", "pool"
// into a DiscoveryScope. The callerRoleID is used for Mode="self".
// allowPool controls whether Mode="pool" is permitted.
func ParseScope(scopeStr, callerRoleID string, allowPool bool) (DiscoveryScope, error) {
	scopeStr = strings.TrimSpace(scopeStr)

	switch {
	case scopeStr == "self" || scopeStr == "":
		return DiscoveryScope{Mode: "self", RoleID: callerRoleID, AllowPool: allowPool}, nil

	case strings.HasPrefix(scopeStr, "role:"):
		roleID := strings.TrimPrefix(scopeStr, "role:")
		roleID = strings.TrimSpace(roleID)
		if roleID == "" {
			return DiscoveryScope{}, fmt.Errorf("scope 'role:' requires a role ID")
		}
		return DiscoveryScope{Mode: "role", RoleID: roleID, AllowPool: allowPool}, nil

	case scopeStr == "pool":
		return DiscoveryScope{Mode: "pool", AllowPool: allowPool}, nil

	default:
		return DiscoveryScope{}, fmt.Errorf("invalid scope %q (must be self, role:<id>, or pool)", scopeStr)
	}
}

// ── FindSkills ────────────────────────────────────────────────────────────

// FindSkillsResult is the return type of FindSkillsImpl.
type FindSkillsResult struct {
	Skills []SkillMatch `json:"skills"`
	Total  int          `json:"total"`
}

// SkillMatch is a single skill match with relevance indicator.
type SkillMatch struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Capability  string `json:"capability"`
	Description string `json:"description"`
	Provides    []string `json:"provides,omitempty"`
	MatchReason string  `json:"match_reason"` // "capability" | "provides" | "name" | "description"
}

// FindSkillsImpl searches the skill registry for skills that can supplement
// a role's capabilities. This is the fast tier-1 path before the slow tier-2
// path (RoleGenerator cookbook LLM call).
//
// At least one of query/capability/provides should be non-empty.
// When capability is non-empty, it matches Skill.Capability exactly.
// When provides is non-empty, it matches any element in Skill.Provides.
// When query is non-empty, it matches Skill.Name or Skill.Description (substring).
//
// Multiple filters can be combined — a skill matching any filter is included.
// Results are sorted by match priority: capability > provides > name > description.
func FindSkillsImpl(reg *config.Registry, query, capability, provides string, page, pageSize int) (*FindSkillsResult, error) {
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if page <= 0 {
		page = 1
	}

	queryLower := strings.ToLower(query)
	providesLower := strings.ToLower(provides)

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	var matched []SkillMatch
	for _, sk := range reg.Skills {
		if sk == nil {
			continue
		}

		matchReason := ""
		// Priority 1: exact capability match
		if capability != "" && sk.Capability == capability {
			matchReason = "capability"
		}
		// Priority 2: provides tag match
		if matchReason == "" && providesLower != "" {
			for _, p := range sk.Provides {
				if strings.ToLower(p) == providesLower {
					matchReason = "provides"
					break
				}
			}
		}
		// Priority 3: name match
		if matchReason == "" && queryLower != "" {
			if strings.Contains(strings.ToLower(sk.Name), queryLower) {
				matchReason = "name"
			}
		}
		// Priority 4: description match
		if matchReason == "" && queryLower != "" {
			if strings.Contains(strings.ToLower(sk.Description), queryLower) {
				matchReason = "description"
			}
		}

		if matchReason != "" {
			matched = append(matched, SkillMatch{
				ID:          sk.ID,
				Name:        sk.Name,
				Capability:  sk.Capability,
				Description: sk.Description,
				Provides:    sk.Provides,
				MatchReason: matchReason,
			})
		}
	}

	// Sort by match priority (capability first, then provides, name, description)
	priority := map[string]int{"capability": 0, "provides": 1, "name": 2, "description": 3}
	sort.SliceStable(matched, func(i, j int) bool {
		return priority[matched[i].MatchReason] < priority[matched[j].MatchReason]
	})

	total := len(matched)
	start := (page - 1) * pageSize
	if start >= total {
		return &FindSkillsResult{Total: total}, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	return &FindSkillsResult{
		Skills: matched[start:end],
		Total:  total,
	}, nil
}
