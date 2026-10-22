package tools

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
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
		Description: "Execute an MCP tool directly using a role's permissions. Records success/failure for system learning.",
		Parameters: map[string]any{
			"role_id":   "string (required) - Role to use for tool access validation",
			"tool":      "string (required) - Full tool name (e.g. 'git.status')",
			"arguments": "object (required) - Map of tool arguments",
			"intent":    "string (optional) - The intent behind this call, used for shadow learning",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			roleID := strArg(args, "role_id")
			toolName := strArg(args, "tool")
			intent := strArg(args, "intent")

			var toolArgs map[string]any
			if raw, ok := args["arguments"].(map[string]any); ok {
				toolArgs = raw
			} else {
				toolArgs = make(map[string]any)
			}

			if roleID == "" || toolName == "" {
				return nil, fmt.Errorf("missing required fields: 'role_id' and 'tool'")
			}

			// 1. Resolve Role & Bindings
			hub := core.NewContextHub(app.Registry, nil, app.ExpStore)
			role := hub.GetRole(roleID)
			if role == nil {
				// Fallback to checking session roles if any (though subsystems usually don't have task context)
				return nil, fmt.Errorf("role %q not found", roleID)
			}

			bindings, err := core.ResolveFinalMCPs(hub, roleID, nil, nil, true) // proxy is admin-only
			if err != nil {
				return nil, fmt.Errorf("failed to resolve MCP bindings for role %q: %w", roleID, err)
			}

			// 2. Locate Tool
			spawner := app.Scheduler.GetSpawner()
			mcpID := spawner.ResolveToolMCP(toolName, bindings, hub)
			if mcpID == "" {
				return nil, fmt.Errorf("tool %q not found or not accessible to role %q", toolName, roleID)
			}

			// 3. Direct Execution
			res, err := spawner.DirectExecute(ctx, mcpID, toolName, toolArgs)

			// 4. Shadow Learning: Record experience asynchronously
			if app.ExpStore != nil {
				go func() {
					// audit R-3: recover prevents panic in shadow learning from crashing process.
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(os.Stderr, "PANIC in shadow learning: %v\n", r)
						}
					}()
					// audit H2: bounded timeout prevents goroutine from
					// hanging forever if RecordTaskCompletion blocks.
					shadowCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					status := "ok"
					if err != nil {
						status = "error"
					}

					// Map tool call to a StepRecord for the Experience Store
					record := store.StepRecord{
						RoleID:     roleID,
						Task:       intent,
						Status:     status,
						Capability: role.BaseCapability,
						Confidence: 1.0, // High-performance agents are assumed confident
					}
					if intent == "" {
						record.Task = fmt.Sprintf("Direct tool call: %s", toolName)
					}

					shadowTaskID := "shadow_" + uuid.New().String()[:8]
					_ = app.ExpStore.RecordTaskCompletion(shadowCtx, shadowTaskID, []store.StepRecord{record}, 1.0, nil, err == nil, false)
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
