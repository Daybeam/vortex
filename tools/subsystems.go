package tools

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Action defines a specific operation within a subsystem.
type Action struct {
	Name        string
	Description string
	Parameters  map[string]any // Simplified parameter info for documentation
	Handler     func(ctx context.Context, app *App, args map[string]any) (any, error)
}

// Subsystem groups related actions together.
type Subsystem struct {
	Name        string
	Description string
	Actions     map[string]Action
}

// SubsystemRegistry manages all registered subsystems.
type SubsystemRegistry struct {
	Subsystems map[string]*Subsystem
}

func NewSubsystemRegistry() *SubsystemRegistry {
	return &SubsystemRegistry{
		Subsystems: make(map[string]*Subsystem),
	}
}

func (r *SubsystemRegistry) Register(s *Subsystem) {
	r.Subsystems[s.Name] = s
}

// Global registry instance
var registry = NewSubsystemRegistry()

// tier1AllowedActions is the set of subsystem actions safe for Tier1 (public SSE) callers.
// Anything not in this map is Tier2-only by default.
var tier1AllowedActions = map[string]bool{
	"config.get_config":     true,
	"config.list_roles":     true,
	"config.list_skills":    true,
	"config.list_mcps":      true,
	"config.list_sops":      true,
	"config.list_providers": true,
	"config.list_models":    true,
	"config.query_groups":   true,
	"config.dispatch_group": true,
	// discovery.discover is safe for Tier1 because scope="self" (the default)
	// only returns the caller's own role's tools. scope="pool" requires admin.
	"discovery.discover":    true,
	"discovery.findskills":  true,
	"exp.get_brief":         true,
	"exp.get_summary":       true,
	"admin.list_tasks":      true,
	"admin.get_step_result": true,
	"admin.get_stats":       true,
	"schedule.list":         true,
	"jit.list_scripts":      true,
	"jit.list_active":       true,
	"jit.list_runtimes":     true,
	"capability.inspect":    true,
	// jit.eval is safe for Tier1 because JITSession enforces sandbox (256MB/CPU limit)
	// and embedded runs are lint-gated + use a fresh VM per call.
	"jit.eval": true,
}

// isActionAllowed checks whether a given action in a subsystem is permitted for the caller's Tier.
func isActionAllowed(subsystem, action, tier string) bool {
	if tier == TierAdmin {
		return true
	}
	return tier1AllowedActions[subsystem+"."+action]
}

// discoverActions returns detailed documentation for actions within a subsystem.
// When called from Tier1 (public SSE), only whitelist-safe actions are shown.
func (r *SubsystemRegistry) discoverActions(name string, tier string) (string, error) {
	s, ok := r.Subsystems[name]
	if !ok {
		return "", fmt.Errorf("subsystem %q not found", name)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Subsystem: %s\n", s.Name))
	sb.WriteString(fmt.Sprintf(">%s\n\n", s.Description))
	sb.WriteString("### Available Actions\n\n")

	keys := make([]string, 0, len(s.Actions))
	for k := range s.Actions {
		if isActionAllowed(name, k, tier) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		a := s.Actions[k]
		sb.WriteString(fmt.Sprintf("#### `%s`\n", a.Name))
		sb.WriteString(fmt.Sprintf("%s\n", a.Description))
		if len(a.Parameters) > 0 {
			sb.WriteString("- **Parameters**:\n")
			for pName, pInfo := range a.Parameters {
				sb.WriteString(fmt.Sprintf("  - `%s`: %v\n", pName, pInfo))
			}
		}
		sb.WriteString("\n---\n")
	}
	return sb.String(), nil
}

// Invoke executes an action in a subsystem with a built-in timeout safeguard.
func (r *SubsystemRegistry) Invoke(ctx context.Context, app *App, subsystem, action string, args map[string]any) (any, error) {
	s, ok := r.Subsystems[subsystem]
	if !ok {
		return nil, fmt.Errorf("subsystem %q not found", subsystem)
	}
	a, ok := s.Actions[action]
	if !ok {
		return nil, fmt.Errorf("action %q not found in subsystem %q", action, subsystem)
	}

	// Runtime tier check: Tier1 callers can only invoke whitelisted actions.
	// Read the tier from the context (authoritative) so stdio callers, which
	// loggedHandler marks as TierAdmin in ctx while leaving app.Tier empty,
	// are not silently demoted to the Tier1 allowlist.
	if !isActionAllowed(subsystem, action, EffectiveTier(ctx, app)) {
		return nil, fmt.Errorf("permission denied: action %q in subsystem %q requires Tier2 (Admin) privileges", action, subsystem)
	}

	// Use a derived context with a safety timeout (default 60s for admin/proxy tasks)
	// Some tools like browser automation might need more time, but we shouldn't hang forever.
	timeout := 60 * time.Second
	if t, ok := args["_timeout_ms"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Millisecond
	}

	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Track start for debugging
	startTime := time.Now()
	if app.Logger != nil {
		app.Logger.Log("EventInvokeStarted", "", "", map[string]any{
			"subsystem": subsystem,
			"action":    action,
		})
	}

	// Execute handler in a channel to support timeout even if handler doesn't check ctx
	type result struct {
		data any
		err  error
	}
	resChan := make(chan result, 1)

	go func() {
		// audit R-2: recover prevents panic in handler from crashing the process.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "PANIC in subsystem handler: %v\n", r)
			}
		}()
		data, err := a.Handler(ictx, app, args)
		resChan <- result{data, err}
	}()

	select {
	case res := <-resChan:
		duration := time.Since(startTime)
		if app.Logger != nil {
			status := "ok"
			if res.err != nil {
				status = "error"
			}
			app.Logger.Log("EventInvokeFinished", "", "", map[string]any{
				"subsystem": subsystem,
				"action":    action,
				"status":    status,
				"duration":  duration.String(),
			})
		}
		// Fire-and-forget usage tracking: promote frequently-used actions
		if res.err == nil && app.Promoter != nil {
			// audit R-4: recover prevents panic in Promoter.Record from crashing process.
			go func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(os.Stderr, "PANIC in Promoter.Record: %v\n", r)
					}
				}()
				app.Promoter.Record(subsystem, action, a)
			}()
		}
		return res.data, res.err
	case <-ictx.Done():
		duration := time.Since(startTime)
		err := ictx.Err()
		if app.Logger != nil {
			app.Logger.Log("EventInvokeTimeout", "", "", map[string]any{
				"subsystem": subsystem,
				"action":    action,
				"duration":  duration.String(),
				"error":     err.Error(),
			})
		}
		return nil, fmt.Errorf("invoke %s.%s timed out or cancelled after %v: %w", subsystem, action, duration, err)
	}
}

// RegisterSubsystems populates the registry with all migrated tools.
func RegisterSubsystems(app *App) {
	registerConfigSubsystem(app)
	registerDiscoverySubsystem(app)
	registerExperienceSubsystem(app)
	registerScheduleSubsystem(app)
	registerAdminSubsystem(app)
	registerJitSubsystem(app)
	registerCapabilitySubsystem(app)
	registerIntelSubsystem(app)
	registerProxySubsystem(app)
	registerModelsSubsystem(app)
}
