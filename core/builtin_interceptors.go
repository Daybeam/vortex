package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/daybeam/vortex/config"
)

// LogInterceptor is a simple interceptor that logs the start and end of a task.
func LogInterceptor(registry *config.Registry, logger *Logger, generator *RoleGenerator) Interceptor {
	return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
		// Resolve role existence via the tiered Hub (session-scoped ephemeral roles
		// first, then the global registry) rather than indexing registry.Roles
		// directly -- so a role already supplied via session_roles, or generated
		// ephemerally earlier in this same task, is correctly recognized instead
		// of triggering a redundant/conflicting generation attempt.
		roleExists := false
		if req.Hub != nil {
			roleExists = req.Hub.GetRole(req.RoleID) != nil
		} else {
			_, roleExists = registry.Roles[req.RoleID]
		}

		if !roleExists {
			// FIX (2026-08-06): If both RoleID and Task are empty, we cannot generate a role.
			// Fail early with a clear error to prevent recursive/meaningless attempts and
			// "role "" not found" downstream failures.
			if req.RoleID == "" && req.Task == "" {
				return nil, fmt.Errorf("Blocked: both role_id and task are empty, cannot resolve or generate a role for step %s", req.StepID)
			}

			switch {
			case registry.EnableEphemeralRoleGen && req.Hub != nil:
				// Ephemeral path (ADDED 2026-07-30): generate a role scoped to this
				// task only. Uses RoleGenerator.GenerateRoleObjects, which -- unlike
				// GetOrCreateRole below -- never touches the shared registry or
				// calls Persist(). The result is stored in req.Hub.Graph's session
				// IR (mutex-guarded, safe under DirectedEngine's concurrent-step
				// execution), exactly like a caller-supplied session_roles entry.
				logger.Log("EventEphemeralRoleGeneration", req.TaskID, req.StepID, map[string]any{
					"message": fmt.Sprintf("Role %s not found, attempting ephemeral (session-scoped) generation", req.RoleID),
				})
			role, skill, err := generator.GenerateRoleObjects(ctx, req.TaskID, req.StepID, req.RoleID, req.Task)
			if err != nil {
				logger.Log("EventEphemeralRoleGenerationFailed", req.TaskID, req.StepID, map[string]any{
					"error": err.Error(),
				})
				if tryDefaultRoleFallback(registry, logger, req, err) {
					break
				}
				return nil, fmt.Errorf("role %q not found and ephemeral generation failed: %w", req.RoleID, err)
			}
				req.Hub.Graph.SetSessionRole(req.RoleID, role)
				if skill != nil {
					req.Hub.Graph.SetSessionSkill(skill.ID, skill)
				}
				logger.Log("EventEphemeralRoleGenerated", req.TaskID, req.StepID, map[string]any{
					"role_id": req.RoleID,
				})
			case registry.EnableDynamicRoleGen:
				logger.Log("EventAutoRoleGeneration", req.TaskID, req.StepID, map[string]any{
					"message": fmt.Sprintf("Role %s not found, attempting dynamic generation", req.RoleID),
				})
				// Use the actual task description to make the role more accurate
			_, err := generator.GetOrCreateRole(ctx, req.TaskID, req.StepID, req.RoleID, req.Task, "code")
			if err != nil {
				logger.Log("EventAutoRoleGenerationFailed", req.TaskID, req.StepID, map[string]any{
					"error": err.Error(),
				})
				if tryDefaultRoleFallback(registry, logger, req, err) {
					break
				}
				return nil, fmt.Errorf("role %q not found and dynamic generation failed: %w", req.RoleID, err)
			}
		default:
			if !tryDefaultRoleFallback(registry, logger, req, fmt.Errorf("dynamic generation is disabled")) {
				return nil, fmt.Errorf("role %q not found and dynamic generation is disabled", req.RoleID)
			}
		}
		}

		logger.Log(EventStepStarted, req.TaskID, req.StepID, map[string]any{
			"message": fmt.Sprintf("Interceptor: Optimizing prompt and preparing context for role %s (Step %s)", req.RoleID, req.StepID),
			"role":    req.RoleID,
		})

		// Call the next handler in the chain
		res, err := next(ctx, req)

		if err != nil {
			logger.Log(EventStepFailed, req.TaskID, req.StepID, map[string]any{
				"message": fmt.Sprintf("Interceptor: Execution failed for role %s", req.RoleID),
				"error":   err.Error(),
			})
			return res, err
		}

		logger.Log(EventStepCompleted, req.TaskID, req.StepID, map[string]any{
			"message": fmt.Sprintf("Interceptor: Execution completed for role %s with status %s", req.RoleID, res.Output.Status),
			"status":  res.Output.Status,
		})

		return res, err
	}
}

// tryDefaultRoleFallback attempts to fall back to the built-in
// "orchestrator_default" role when dynamic/ephemeral role generation fails
// (e.g. no cookbook source configured, weak model, network error). This
// preserves orchestrator value (interceptors, context assembly, loop guard,
// Sieve Guardian) instead of surfacing a hard error that forces the caller
// to fall back to raw LLM with zero orchestrator benefit.
//
// Returns true if the fallback succeeded (caller should break/continue),
// false if no default role is available (caller should return the error).
func tryDefaultRoleFallback(registry *config.Registry, logger *Logger, req *SpawnRequest, genErr error) bool {
	defaultRole := registry.Roles["orchestrator_default"]
	if defaultRole == nil {
		return false
	}
	logger.Log("EventRoleFallbackToDefault", req.TaskID, req.StepID, map[string]any{
		"original_role_id": req.RoleID,
		"error":            genErr.Error(),
	})
	if req.Hub != nil && req.Hub.Graph != nil {
		req.Hub.Graph.SetSessionRole(req.RoleID, defaultRole)
	} else {
		req.RoleID = "orchestrator_default"
	}
	return true
}

// HealthCheckInterceptor verifies that local MCP commands exist before execution.
// Missing MCP dependencies are degraded (skipped) rather than blocking the entire step.
// This allows a role with multiple MCP bindings to continue functioning even when
// one MCP's binary is unavailable (e.g. pyright/lsmcp not installed on Linux).
func HealthCheckInterceptor(registry *config.Registry, logger *Logger) Interceptor {
	return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
		role := registry.Roles[req.RoleID]
		if role != nil {
			mcpIDs := role.BoundMCPIDs()
			mcpIDs = append(mcpIDs, req.AdditionalMCPs...)
			var degradedMCPs []string
			for _, mcpID := range mcpIDs {
				if mcpDef := registry.GetMCP(mcpID); mcpDef != nil && mcpDef.Command != "" {
					cmdName := strings.Fields(mcpDef.Command)[0]
					if _, err := exec.LookPath(cmdName); err != nil {
						logger.Log("EventMCPDependencyMissing", req.TaskID, req.StepID, map[string]any{
							"message": fmt.Sprintf("Missing dependency for MCP %s: %s", mcpID, cmdName),
							"mcp_id":  mcpID,
						})

						// VDA Self-healing: only attempt auto-repair when explicitly enabled.
						// This is a safety gate (F25/F26 risk class): auto-executing
						// shell scripts in the routine task path must be opt-in.
						if registry.System.EnableAutoRepair {
							// VDA Self-healing attempt: check for fix script in scripts/fix/
							fixScript := filepath.Join("scripts", "fix", fmt.Sprintf("fix_%s.bat", cmdName))
							if runtime.GOOS != "windows" {
								fixScript = filepath.Join("scripts", "fix", fmt.Sprintf("fix_%s.sh", cmdName))
							}

							if _, ferr := os.Stat(fixScript); ferr == nil {
								logger.Log("EventEnvironmentRepairStarted", req.TaskID, req.StepID, map[string]any{
									"dependency": cmdName,
									"script":     fixScript,
								})

								var repairCmd *exec.Cmd
								if runtime.GOOS == "windows" {
									repairCmd = exec.Command("cmd", "/C", fixScript)
								} else {
									repairCmd = exec.Command("sh", fixScript)
								}

								if rout, rerr := repairCmd.CombinedOutput(); rerr == nil {
									logger.Log("EventEnvironmentRepairSuccess", req.TaskID, req.StepID, map[string]any{
										"dependency": cmdName,
										"output":     string(rout),
									})
									// Re-check after repair
									if _, err := exec.LookPath(cmdName); err == nil {
										continue // Fixed!
									}
								} else {
									logger.Log("EventEnvironmentRepairFailed", req.TaskID, req.StepID, map[string]any{
										"dependency": cmdName,
										"error":      rerr.Error(),
										"output":     string(rout),
									})
								}
							}
						}

						// Degrade: skip this MCP instead of blocking the entire step.
						// The role can still function with its remaining MCPs and skills.
						logger.Log("EventMCPDegraded", req.TaskID, req.StepID, map[string]any{
							"mcp_id":  mcpID,
							"command": cmdName,
							"action":  "skipped",
						})
						degradedMCPs = append(degradedMCPs, mcpID)
						continue
					}
				}
			}
			// Remove degraded MCPs from the request so downstream spawner doesn't try to start them
			if len(degradedMCPs) > 0 {
				req.DegradedMCPs = degradedMCPs
				// Filter out degraded MCPs from AdditionalMCPs
				var filtered []string
				for _, m := range req.AdditionalMCPs {
					skip := false
					for _, d := range degradedMCPs {
						if m == d {
							skip = true
							break
						}
					}
					if !skip {
						filtered = append(filtered, m)
					}
				}
				req.AdditionalMCPs = filtered
			}
		}
		return next(ctx, req)
	}
}
