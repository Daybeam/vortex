package tools

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/core"
)

// subsystems_discovery.go — Register the `discover` MCP tool.
//
// Implements docs/plan/TWO_LAYER_SCOPED_DISCOVERY_DESIGN.md §4.3.
//
// The `discover` tool is the unified front-end for tool, role, and MCP
// discovery with scope enforcement. It replaces the need for separate
// list_mcps / list_roles / SearchTools calls when the caller wants scoped
// discovery (e.g. "what tools can MY role see?" vs "what tools exist?").
//
// Phase 1: reads from in-memory registry (no tools/list I/O).
// Phase 2 will add lazy tools/list for MCPs with empty FullToolDefinitions.

func registerDiscoverySubsystem(app *App) {
	s := &Subsystem{
		Name:        "discovery",
		Description: "Scoped discovery of tools, roles, and MCPs within a visibility boundary.",
		Actions:     make(map[string]Action),
	}

	s.Actions["discover"] = Action{
		Name: "discover",
		Description: "Discover available tools, roles, or MCPs within a scope. " +
			"scope='self' discovers your own role's tools (default, safe for Tier1). " +
			"scope='role:<id>' discovers another role's tools (planner use). " +
			"scope='pool' discovers everything (admin only).",
		Parameters: map[string]any{
			"scope":     "string (required) - self | role:<roleID> | pool (default: self)",
			"type":      "string (required) - tools | roles | mcps",
			"query":     "string (optional) - keyword filter (case-insensitive)",
			"page":      "int (optional, default 1)",
			"page_size": "int (optional, default 20, max 100)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			scopeStr := strArg(args, "scope")
			if scopeStr == "" {
				scopeStr = "self"
			}
			dtype := strArg(args, "type")
			if dtype == "" {
				return nil, fmt.Errorf("parameter 'type' is required (tools, roles, or mcps)")
			}
			query := strArg(args, "query")
			page := intArg(args, "page", 1)
			pageSize := intArg(args, "page_size", 20)

			// Determine caller's admin status from the context (authoritative).
			// SECURITY FIX (2026-10-21): _role_id and _is_admin were previously
			// read from args, which are caller-controlled. The invoke dispatcher
			// now strips _-prefixed keys, but we also harden here: identity/tier
			// come ONLY from ctx. callerRoleID is empty because the invoke
			// dispatcher does not inject task-level role context into subsystem
			// calls (if needed in the future, inject via ctx.Value, never args).
			callerRoleID := ""
			isAdmin := EffectiveTier(ctx, app) == TierAdmin

			// Parse scope string into DiscoveryScope.
			scope, err := core.ParseScope(scopeStr, callerRoleID, isAdmin)
			if err != nil {
				return nil, err
			}

			// For scope="self" without a caller role ID, return empty rather
			// than escalating. SECURITY FIX (2026-10-21): previously, when
			// type=roles, this silently upgraded to pool scope (AllowPool: true),
			// contradicting the "scope = security boundary" principle and the
			// audit L-20-2 fix preventing public enumeration of all roles.
			// If the main agent needs to list roles, use explicit scope=pool
			// with admin tier, not a silent escalation from self.
			if scope.Mode == "self" && scope.RoleID == "" {
				return &core.DiscoveryResult{Total: 0}, nil
			}

			result, err := core.DiscoverImpl(app.Registry, scope, dtype, query, page, pageSize)
			if err != nil {
				return nil, err
			}
			return result, nil
		},
	}

	s.Actions["findskills"] = Action{
		Name: "findskills",
		Description: "Find existing skills that can supplement a role's capabilities. " +
			"Searches the skill registry by capability, name, description, or provides tags. " +
			"Use when a role lacks required capabilities — findskills is the fast tier-1 path " +
			"before the slow tier-2 path (RoleGenerator cookbook LLM call).",
		Parameters: map[string]any{
			"query":      "string (optional) - keyword to search in skill name/description",
			"capability": "string (optional) - exact capability match (e.g. 'code', 'analysis')",
			"provides":   "string (optional) - match against skill.Provides tags",
			"page":       "int (optional, default 1)",
			"page_size":  "int (optional, default 20, max 100)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			query := strArg(args, "query")
			capability := strArg(args, "capability")
			provides := strArg(args, "provides")
			page := intArg(args, "page", 1)
			pageSize := intArg(args, "page_size", 20)

			result, err := core.FindSkillsImpl(app.Registry, query, capability, provides, page, pageSize)
			if err != nil {
				return nil, err
			}
			return result, nil
		},
	}

	registry.Register(s)
}
