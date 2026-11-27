package tools

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/store"
)

func registerProxySubsystem(app *App) {
	s := &Subsystem{
		Name:        "proxy",
		Description: "Direct tool execution for high-performance session agents. Orchestrator acts as a resource gateway.",
		Actions:     make(map[string]Action),
	}

	s.Actions["call"] = Action{
		Name:        "call",
		Description: "Execute an MCP tool directly. When role_id is provided, uses that role's permissions; when omitted, resolves globally via mount policy gate.",
		Parameters: map[string]any{
			"role_id":   "string (optional) - Role to use for tool access validation. Omit for global resolution with mount policy gate.",
			"tool":      "string (required) - Full tool name (e.g. 'git.status')",
			"arguments": "object (required) - Map of tool arguments",
			"intent":    "string (optional) - The intent behind this call, used for shadow learning",
			"mcp_id":    "string (optional) - Disambiguate when multiple MCPs expose the same tool name",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			roleID := strArg(args, "role_id")
			toolName := strArg(args, "tool")
			intent := strArg(args, "intent")
			mcpIDArg := strArg(args, "mcp_id")

			var toolArgs map[string]any
			if raw, ok := args["arguments"].(map[string]any); ok {
				toolArgs = raw
			} else {
				toolArgs = make(map[string]any)
			}

			if toolName == "" {
				return nil, fmt.Errorf("missing required field: 'tool'")
			}

			hub := core.NewContextHub(app.Registry, nil, app.ExpStore)
			spawner := app.Scheduler.GetSpawner()

			var mcpID string
			if roleID != "" {
				// Role-based path (existing behavior, backward compatible)
				role := hub.GetRole(roleID)
				if role == nil {
					return nil, fmt.Errorf("role %q not found", roleID)
				}
				bindings, err := core.ResolveFinalMCPs(hub, roleID, nil, nil, true)
				if err != nil {
					return nil, fmt.Errorf("failed to resolve MCP bindings for role %q: %w", roleID, err)
				}
				mcpID = spawner.ResolveToolMCP(toolName, bindings, hub)
				if mcpID == "" {
					return nil, fmt.Errorf("tool %q not found or not accessible to role %q", toolName, roleID)
				}
			} else {
				// Roleless path: global resolution + mount policy gate
				var err error
				mcpID, err = resolveRolelessMCP(ctx, app, hub, spawner, toolName, mcpIDArg)
				if err != nil {
					return nil, err
				}
			}

			// Direct Execution
			res, err := spawner.DirectExecute(ctx, mcpID, toolName, toolArgs)

		// Shadow Learning: only for role-based path (roleless skips attribution)
		if roleID != "" && app.ExpStore != nil {
			// audit R2: track shadow learning goroutine with shadowWg so
			// Shutdown can wait for pending learning writes to drain.
			app.shadowWg.Add(1)
			go func() {
				defer app.shadowWg.Done()
				defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(os.Stderr, "PANIC in shadow learning: %v\n", r)
						}
					}()
					shadowCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					status := "ok"
					if err != nil {
						status = "error"
					}

					role := hub.GetRole(roleID)
					capability := ""
					if role != nil {
						capability = role.BaseCapability
					}
					record := store.StepRecord{
						RoleID:     roleID,
						Task:       intent,
						Status:     status,
						Capability: capability,
						Confidence: 1.0,
					}
					if intent == "" {
						record.Task = fmt.Sprintf("Direct tool call: %s", toolName)
					}

				shadowTaskID := "shadow_" + uuid.New().String()[:8]
				if err := app.ExpStore.RecordTaskCompletion(shadowCtx, shadowTaskID, []store.StepRecord{record}, 1.0, nil, err == nil, false); err != nil {
					fmt.Fprintf(os.Stderr, "WARN: shadow learning: RecordTaskCompletion failed: %v\n", err)
				}
				}()
			}

			if err != nil {
				return nil, err
			}
			return res, nil
		},
	}

	registry.Register(s)
}

// resolveRolelessMCP finds the MCP that owns toolName without a role binding.
// It scans all registry MCPs, applies disambiguation rules, and checks the
// mount policy gate. See docs/completed/2026-11-12/PROXY_CALL_ROLELESS_RESOLUTION_DESIGN.md.
func resolveRolelessMCP(ctx context.Context, app *App, hub *core.ContextHub, spawner *core.Spawner, toolName, mcpIDArg string) (string, error) {
	// mcp_id provided: direct locate, still check toolEnabled + ToolFilter + gate
	if mcpIDArg != "" {
		mcp := hub.GetMCP(mcpIDArg)
		if mcp == nil {
			return "", fmt.Errorf("MCP %q not found", mcpIDArg)
		}
		if mcp.Disabled {
			return "", fmt.Errorf("MCP %q is disabled", mcpIDArg)
		}
		if slices.Contains(mcp.DisabledTools, toolName) {
			return "", fmt.Errorf("tool %q is disabled on MCP %q", toolName, mcpIDArg)
		}
		if mcp.ToolFilter != nil && !mcp.ToolFilter.Test(toolName) {
			return "", fmt.Errorf("tool %q is not allowed by MCP %q tool filter", toolName, mcpIDArg)
		}
		if err := checkRolelessGate(hub, mcpIDArg, EffectiveTier(ctx, app)); err != nil {
			return "", err
		}
		return mcpIDArg, nil
	}

	// No mcp_id: scan all MCPs for exact tool name matches
	candidates := findToolCandidates(toolName, hub)
	if len(candidates) > 1 {
		var parts []string
		for _, c := range candidates {
			policy := "admin_only"
			if def := hub.GetMCP(c); def != nil {
				policy = def.EffectiveMountPolicy()
			}
			parts = append(parts, fmt.Sprintf("  %s (mount_policy: %s)", c, policy))
		}
		return "", fmt.Errorf("tool %q is available on multiple MCPs:\n%s\nprovide mcp_id or role_id to disambiguate",
			toolName, strings.Join(parts, "\n"))
	}

	var mcpID string
	if len(candidates) == 1 {
		mcpID = candidates[0]
	} else {
		// No exact match: try ResolveToolMCP with all bindings for Tier 3 fallback
		allBindings := buildAllMCPBindings(hub)
		mcpID = spawner.ResolveToolMCP(toolName, allBindings, hub)
		if mcpID == "" {
			return "", fmt.Errorf("tool %q not found; use orchestrator_discover to list available tools", toolName)
		}
	}

	if err := checkRolelessGate(hub, mcpID, EffectiveTier(ctx, app)); err != nil {
		return "", err
	}
	return mcpID, nil
}

// buildAllMCPBindings creates virtual unrestricted bindings for every active
// MCP in the registry (both static and dynamic), enabling ResolveToolMCP's
// existing resolution logic to operate without a role.
func buildAllMCPBindings(hub *core.ContextHub) []config.MCPBinding {
	hub.Registry.Mu.RLock()
	defer hub.Registry.Mu.RUnlock()

	var bindings []config.MCPBinding
	for id, mcp := range hub.Registry.MCPs {
		if mcp == nil || mcp.Disabled {
			continue
		}
		bindings = append(bindings, config.MCPBinding{MCPID: id})
	}
	for id, mcp := range hub.Registry.DynamicMCPs {
		if mcp == nil || mcp.Disabled {
			continue
		}
		bindings = append(bindings, config.MCPBinding{MCPID: id})
	}
	return bindings
}

// findToolCandidates returns MCP IDs that explicitly declare toolName in their
// AvailableTools, applying the same checks as ResolveToolMCP Tier 1: the MCP
// must be active, the tool must not be disabled, and the tool filter must pass.
func findToolCandidates(toolName string, hub *core.ContextHub) []string {
	hub.Registry.Mu.RLock()
	defer hub.Registry.Mu.RUnlock()

	var candidates []string
	check := func(mcp *config.MCPDef) {
		if mcp == nil || mcp.Disabled {
			return
		}
		if slices.Contains(mcp.DisabledTools, toolName) {
			return
		}
		if mcp.ToolFilter != nil && !mcp.ToolFilter.Test(toolName) {
			return
		}
		if slices.Contains(mcp.AvailableTools, toolName) {
			candidates = append(candidates, mcp.ID)
		}
	}
	for _, mcp := range hub.Registry.MCPs {
		check(mcp)
	}
	for _, mcp := range hub.Registry.DynamicMCPs {
		check(mcp)
	}
	return candidates
}

// checkRolelessGate applies the mount policy gate for the roleless path.
// Admin tier passes all policies; non-admin is subject to open/approval/admin_only.
// Dynamic MCPs that are server-risky are rejected regardless of tier.
func checkRolelessGate(hub *core.ContextHub, mcpID, tier string) error {
	mcp := hub.GetMCP(mcpID)
	if mcp == nil {
		return fmt.Errorf("MCP %q not found", mcpID)
	}

	// Dynamic MCPs that are server-risky are rejected in the roleless path
	hub.Registry.Mu.RLock()
	_, isDynamic := hub.Registry.DynamicMCPs[mcpID]
	hub.Registry.Mu.RUnlock()
	if isDynamic && config.ServerRiskyMCP(mcp) {
		return fmt.Errorf("MCP %q is server-risky; provide role_id to use it explicitly", mcpID)
	}

	if tier == TierAdmin {
		return nil
	}

	switch mcp.EffectiveMountPolicy() {
	case config.MountPolicyOpen:
		return nil
	case config.MountPolicyApproval:
		if hub.Graph != nil && hub.Graph.IsMCPMountApproved(mcpID) {
			return nil
		}
		return fmt.Errorf("MCP %q requires approval (mount_policy: approval)", mcpID)
	default: // admin_only
		return fmt.Errorf("MCP %q requires admin tier (mount_policy: admin_only)", mcpID)
	}
}
