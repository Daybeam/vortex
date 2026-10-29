package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
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
			registry.Mu.RLock()
			_, roleExists = registry.Roles[req.RoleID]
			registry.Mu.RUnlock()
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
				// NOTE (2026-10-18): a tool-surface cloning step briefly lived
				// here (CloneToolSurface + pickToolSurfaceTemplate) and was
				// reverted. It copied another role's BoundMCPBindings onto the
				// generated role, but that is the wrong layer: a step's tool
				// surface is already expressible via StepInput.AdditionalMCPs /
				// AdditionalToolAllowlists, so cloning duplicated the same
				// capability behind an extra object, an extra serialisation into
				// the task manifest, and a name-matching heuristic. Roles decide
				// HOW to think (instruction, synthesised from the cookbook);
				// steps decide WHAT to use. See docs 52 §12.3.
			// FIX (2026-10-27): when the caller supplied RoleID == "" (the
			// "let the system pick a role from the task" flow),
			// GenerateRoleObjects derives a real effectiveRoleID internally
			// (role_generator.go:372) but only exposes it on the returned
			// role object. Storing under the empty key and leaving
			// req.RoleID == "" made GetRole("") return nil unconditionally
			// (hub.go:103), so every empty-RoleID spawn died with
			// `role "" not found` right after generation "succeeded" —
			// observed 2026-10-27 in task_68d6fe80 / task_7e8d85f3 (both
			// reached EventEphemeralRoleGenerated then step_failed).
			// Mirror the E16.10-B1 fix already in tryDefaultRoleFallback:
			// rewrite req.RoleID to the generated role's own ID and cache
			// under that ID so both lookup paths agree.
			if req.RoleID == "" && role.ID != "" {
				req.RoleID = role.ID
			}
			req.Hub.Graph.SetSessionRole(req.RoleID, role)
			if skill != nil {
				req.Hub.Graph.SetSessionSkill(skill.ID, skill)
			}
			logger.Log("EventEphemeralRoleGenerated", req.TaskID, req.StepID, map[string]any{
				"role_id": req.RoleID,
			})
			// Record substitution: ephemeral role generated instead of requested role
			if req.Hub != nil && req.Hub.Graph != nil {
				req.Hub.Graph.RecordSubstitution("ephemeral_role", "", req.RoleID, "role not found, ephemeral generation used", req.StepID)
			}
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
		// audit L-NEW-7: Spawn can return (nil, nil) when an external
		// interceptor short-circuits. Guard to prevent nil deref panic.
		if res == nil {
			return nil, fmt.Errorf("log_interceptor: spawn returned nil result for step %s", req.StepID)
		}

		// A failed status here usually comes from spawner.failedOutput, which
		// returns (result, nil) instead of an error — so the err branch above
		// never fires and the reason is invisible unless we surface it. Without
		// this, a step that dies in 1.6ms with model="" provider="" leaves no
		// trace of why (observed 2026-10-19 on the first multi-step DAG run).
		failedDetail := map[string]any{
			"message": fmt.Sprintf("Interceptor: Execution completed for role %s with status %s", req.RoleID, res.Output.Status),
			"status":  res.Output.Status,
		}
		if res.Output.Status != schemas.StatusOK && res.Output.Status != schemas.StatusPartial {
			if v, ok := res.Output.Result["error"]; ok {
				failedDetail["error"] = fmt.Sprint(v)
			}
			if len(res.Output.Warnings) > 0 {
				failedDetail["warnings"] = res.Output.Warnings
			}
			if len(res.Output.Assumptions) > 0 {
				failedDetail["assumptions"] = res.Output.Assumptions
			}
		}
		logger.Log(EventStepCompleted, req.TaskID, req.StepID, failedDetail)

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
	registry.Mu.RLock()
	defaultRole := registry.Roles["orchestrator_default"]
	registry.Mu.RUnlock()
	if defaultRole == nil {
		return false
	}
	bindingCount := len(defaultRole.BoundMCPIDs())
	logger.Log("EventRoleFallbackToDefault", req.TaskID, req.StepID, map[string]any{
		"original_role_id":   req.RoleID,
		"error":              genErr.Error(),
		"level":              "ERROR",
		"mcp_bindings_count": bindingCount,
	})
	// Record substitution: fell back to orchestrator_default
	if req.Hub != nil && req.Hub.Graph != nil {
		req.Hub.Graph.RecordSubstitution("role_fallback", req.RoleID, "orchestrator_default", genErr.Error(), req.StepID)
	}
	if bindingCount == 0 {
		logger.Log("EventRoleFallbackNoBindings", req.TaskID, req.StepID, map[string]any{
			"role_id": "orchestrator_default",
			"warning": "fallback role has zero MCP bindings; model can only use built-in tools",
			"level":   "WARN",
		})
	}
	// E16.10-B1 fix (2026-10-07): the Graph path previously stored the
	// fallback role under the ORIGINAL role id (often "") and left
	// req.RoleID untouched — ContextHub.GetRole returns nil for ""
	// unconditionally, so every empty-RoleID spawn (e.g. chat
	// delegate_to_orchestrator -> SubmitWithSession without a role) died
	// with `role "" not found` right after this fallback "succeeded".
	// Rewrite req.RoleID unconditionally and cache the role under its own
	// id so both lookup paths agree.
	req.RoleID = defaultRole.ID
	if req.Hub != nil && req.Hub.Graph != nil {
		req.Hub.Graph.SetSessionRole(defaultRole.ID, defaultRole)
	}
	return true
}

// HealthCheckInterceptor verifies that local MCP commands exist before execution.
// Missing MCP dependencies are degraded (skipped) rather than blocking the entire step.
// This allows a role with multiple MCP bindings to continue functioning even when
// one MCP's binary is unavailable (e.g. pyright/lsmcp not installed on Linux).
func HealthCheckInterceptor(registry *config.Registry, logger *Logger) Interceptor {
	return func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
		registry.Mu.RLock()
		role := registry.Roles[req.RoleID]
		registry.Mu.RUnlock()
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
							// audit S-H5: sanitize cmdName to prevent path traversal in fix script path
							safeCmdName := filepath.Base(cmdName)
							fixScript := filepath.Join("scripts", "fix", fmt.Sprintf("fix_%s.bat", safeCmdName))
							if runtime.GOOS != "windows" {
								fixScript = filepath.Join("scripts", "fix", fmt.Sprintf("fix_%s.sh", safeCmdName))
							}

							if _, ferr := os.Stat(fixScript); ferr == nil {
								logger.Log("EventEnvironmentRepairStarted", req.TaskID, req.StepID, map[string]any{
									"dependency": cmdName,
									"script":     fixScript,
								})

								var repairCmd *exec.Cmd
								// audit S-1: use CommandContext with 10s timeout to
								// prevent a hung repair script from blocking the
								// interceptor chain forever.
								repairCtx, repairCancel := context.WithTimeout(ctx, 10*time.Second)
								defer repairCancel()
								if runtime.GOOS == "windows" {
									repairCmd = exec.CommandContext(repairCtx, "cmd", "/C", fixScript)
								} else {
									repairCmd = exec.CommandContext(repairCtx, "sh", fixScript)
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
					// Record substitution: MCP degraded (dependency missing)
					if req.Hub != nil && req.Hub.Graph != nil {
						req.Hub.Graph.RecordSubstitution("mcp_degraded", mcpID, "", "dependency "+cmdName+" not found, MCP skipped", req.StepID)
					}
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
