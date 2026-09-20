package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/codeintel"
	ci "github.com/daybeam/vortex/pkg/codeintel/tools"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/google/uuid"
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

// discoverSubsystems returns a summary of all available subsystems.
func (r *SubsystemRegistry) discoverSubsystems() string {
	var sb strings.Builder
	sb.WriteString("## Vortex Subsystems\n\n")
	sb.WriteString("Use these subsystems for administrative, configuration, and background tasks. ")
	sb.WriteString("Call `orchestrator_discover(subsystem=\"name\")` to see detailed actions.\n\n")

	keys := make([]string, 0, len(r.Subsystems))
	for k := range r.Subsystems {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		s := r.Subsystems[k]
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", s.Name, s.Description))
	}
	return sb.String()
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
	if !isActionAllowed(subsystem, action, app.Tier) {
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
			go app.Promoter.Record(subsystem, action, a)
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
	registerExperienceSubsystem(app)
	registerScheduleSubsystem(app)
	registerAdminSubsystem(app)
	registerJitSubsystem(app)
	registerCapabilitySubsystem(app)
	registerIntelSubsystem(app)
	registerProxySubsystem(app)
	registerModelsSubsystem(app)
}

func registerConfigSubsystem(app *App) {
	s := &Subsystem{
		Name:        "config",
		Description: "Providers, roles, and system-wide settings.",
		Actions:     make(map[string]Action),
	}

	s.Actions["get_config"] = Action{
		Name:        "get_config",
		Description: "Return the full current configuration (parsed JSON, secrets masked).",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			raw, err := app.Registry.LoadRaw()
			if err != nil {
				return nil, err
			}
			var cfg config.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, err
			}
			// Mask API keys
			for name, p := range cfg.Providers {
				if p.APIKey != "" {
					p.APIKey = "***configured***"
					cfg.Providers[name] = p
				}
			}
			// FIX (2026-06-25, re-applied 2026-06-28): cfg.Skills only reflects the main config skills array;
			// not skills loaded from workspace/skills/ directory by Registry.Load.
			// Overwrite with the live in-memory registry state so this matches runtime behavior.
			app.Registry.Mu.RLock()
			liveSkills := make([]config.Skill, 0, len(app.Registry.Skills))
			for _, sk := range app.Registry.Skills {
				if sk != nil {
					liveSkills = append(liveSkills, *sk)
				}
			}
			app.Registry.Mu.RUnlock()
			cfg.Skills = liveSkills
			app.Registry.Mu.RLock()
			liveMCPs := make([]config.MCPDef, 0, len(app.Registry.MCPs))
			for _, m := range app.Registry.MCPs {
				if m != nil {
					liveMCPs = append(liveMCPs, *m)
				}
			}
			liveGroups := make([]config.RoleGroup, 0, len(app.Registry.RoleGroups))
			for _, g := range app.Registry.RoleGroups {
				if g != nil {
					liveGroups = append(liveGroups, *g)
				}
			}
			// FIX (2026-07-24): cfg.Roles only reflects the main config roles array as
			// re-parsed from disk; not roles loaded from a split workspace/roles/
			// directory by Registry.Load (same class of gap as the Skills/MCPs fix
			// above). Overwrite with the live in-memory registry state so this
			// diagnostic endpoint matches what list_roles / real task execution see.
			liveRoles := make([]config.Role, 0, len(app.Registry.Roles))
			for _, r := range app.Registry.Roles {
				if r != nil {
					liveRoles = append(liveRoles, *r)
				}
			}
			app.Registry.Mu.RUnlock()
			cfg.MCPs = liveMCPs
			cfg.RoleGroups = liveGroups
			cfg.Roles = liveRoles
			return cfg, nil
		},
	}

	s.Actions["list_roles"] = Action{
		Name:        "list_roles",
		Description: "List all registered agent roles.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			app.Registry.Mu.RLock()
			defer app.Registry.Mu.RUnlock()
			var roles []config.Role
			for _, r := range app.Registry.Roles {
				if r != nil {
					roles = append(roles, *r)
				}
			}
			return roles, nil
		},
	}

	s.Actions["list_skills"] = Action{
		Name:        "list_skills",
		Description: "List all registered cognitive skills.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			app.Registry.Mu.RLock()
			defer app.Registry.Mu.RUnlock()
			var skills []config.Skill
			for _, s := range app.Registry.Skills {
				if s != nil {
					skills = append(skills, *s)
				}
			}
			return skills, nil
		},
	}

	s.Actions["register_provider"] = Action{
		Name:        "register_provider",
		Description: "Register or update an AI provider. Persists to config.json.",
		Parameters: map[string]any{
			"name":        "string (required)",
			"provider":    "string (required) - anthropic|openai|gemini|ollama",
			"model":       "string (required)",
			"api_key_env": "string - Env var name for API key",
			"base_url":    "string - Override API base URL",
			"instances":   "[]object - Alternate credentials for failover",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			name := strArg(args, "name")
			if name == "" {
				return nil, fmt.Errorf("provider 'name' is required")
			}

			// Use JSON round-trip to cleanly unmarshal into ProviderConfig
			data, _ := json.Marshal(args)
			var pc config.ProviderConfig
			if err := json.Unmarshal(data, &pc); err != nil {
				return nil, fmt.Errorf("invalid provider config: %w", err)
			}

			if pc.Provider == "" || pc.Model == "" {
				return nil, fmt.Errorf("missing required fields: 'provider' and 'model'")
			}

			ops := []config.PatchOp{
				{Op: config.OpAdd, Path: "/providers/" + name, Value: json.RawMessage(data)},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["update_system"] = Action{
		Name:        "update_system",
		Description: "Update global system settings. Persists to config.json.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			var ops []config.PatchOp
			fields := []string{
				"confidence_threshold", "max_decision_outcomes", "max_context_keep",
				"default_jit_ttl", "swarm_fallback_delay", "staging_enabled",
				"bookmarks", "external_interceptors", "swarm_mode_enabled",
			}

			for _, f := range fields {
				if v, ok := args[f]; ok {
					val, _ := json.Marshal(v)
					path := "/system/" + f
					if f == "swarm_mode_enabled" {
						path = "/swarm_mode_enabled"
					}
					ops = append(ops, config.PatchOp{Op: config.OpReplace, Path: path, Value: json.RawMessage(val)})
				}
			}

			if len(ops) == 0 {
				return map[string]any{"ok": true, "message": "no changes"}, nil
			}

			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["update_sop"] = Action{
		Name:        "update_sop",
		Description: "Register or update an SOP (Standard Operating Procedure). Persists to workspace/sops/<id>.json.",
		Parameters: map[string]any{
			"id":          "string (required) - unique SOP identifier",
			"description": "string",
			"steps":       "[]object - array of SOP steps",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			data, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("update_sop: failed to serialize args: %w", err)
			}
			var sop schemas.SOP
			if err := json.Unmarshal(data, &sop); err != nil {
				return nil, fmt.Errorf("update_sop: failed to parse SOP fields: %w", err)
			}
			if sop.ID == "" {
				return nil, fmt.Errorf("update_sop: 'id' is required and must not be empty")
			}

			// Determine the correct sops directory
			sopsDir := app.Registry.GetConfigPath()
			sopsDir = filepath.Join(config.ConfigDir(sopsDir), "workspace/sops/")
			if err := os.MkdirAll(sopsDir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create sops dir: %w", err)
			}

			// Save SOP to individual JSON file, sanitize ID to prevent path traversal
			safeID := filepath.Base(sop.ID)
			sopPath := filepath.Join(sopsDir, safeID+".json")
			sopData, err := json.MarshalIndent(sop, "", "  ")
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(sopPath, sopData, 0644); err != nil {
				return nil, err
			}

			// Update in-memory registry
			app.Registry.Mu.Lock()
			app.Registry.SOPs[sop.ID] = &sop
			app.Registry.Mu.Unlock()

			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["set_default_provider"] = Action{
		Name:        "set_default_provider",
		Description: "Set the default provider.",
		Parameters:  map[string]any{"name": "string (required)"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			name := strArg(args, "name")
			if name == "" {
				return nil, fmt.Errorf("name is required")
			}
			ops := []config.PatchOp{
				{Op: config.OpReplace, Path: "/default_provider", Value: json.RawMessage(`"` + name + `"`)},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["register_mcp"] = Action{
		Name:        "register_mcp",
		Description: "Register an MCP server in the whitelist.",
		Parameters: map[string]any{
			"id":      "string (required)",
			"url":     "string (required)",
			"trusted": "boolean",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			url := strArg(args, "url")
			if id == "" || url == "" {
				return nil, fmt.Errorf("missing id or url")
			}
			data, _ := json.Marshal(args)
			ops := []config.PatchOp{
				{Op: config.OpAdd, Path: "/mcps/" + id, Value: json.RawMessage(data)},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["list_mcps"] = Action{
		Name:        "list_mcps",
		Description: "List all registered MCP servers and their current status.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			app.Registry.Mu.RLock()
			defer app.Registry.Mu.RUnlock()
			var result []map[string]any
			for id, m := range app.Registry.MCPs {
				result = append(result, map[string]any{
					"id":      id,
					"url":     m.URL,
					"trusted": m.Trusted,
					"type":    "static",
				})
			}
			for id, m := range app.Registry.DynamicMCPs {
				result = append(result, map[string]any{
					"id":         id,
					"url":        m.URL,
					"type":       "dynamic",
					"expires_at": m.ExpiresAt.Format(time.RFC3339),
				})
			}
			return result, nil
		},
	}

	s.Actions["register_role"] = Action{
		Name:        "register_role",
		Description: "Register or update a role. Persists to config.json.",
		Parameters: map[string]any{
			"id":              "string (required) - unique role identifier",
			"name":            "string (required)",
			"base_capability": "string (required) - e.g. 'code', 'analysis'",
			"provider":        "string (optional) - override default provider name",
			"bound_skills":    "[]string (optional)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			if id == "" {
				return nil, fmt.Errorf("register_role: 'id' is required and must not be empty")
			}
			data, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("register_role: failed to serialize args: %w", err)
			}

			// FIX (2026-08-31): validate that any bound_skills reference an
			// already-registered skill before committing. This check existed in
			// the pre-CommitOps implementation but was dropped when this handler
			// was rewritten to go through the generic patch.go/CommitOps
			// mechanism, which applies structural JSON-Patch operations via
			// reflection and has no knowledge of cross-referential domain
			// constraints. Without this, a role could reference a nonexistent
			// skill, silently creating a dangling reference that only surfaces
			// later as a confusing runtime failure when the role is actually used.
			var boundSkillsCheck struct {
				BoundSkills []string `json:"bound_skills"`
			}
			if jerr := json.Unmarshal(data, &boundSkillsCheck); jerr == nil && len(boundSkillsCheck.BoundSkills) > 0 {
				app.Registry.Mu.RLock()
				for _, sid := range boundSkillsCheck.BoundSkills {
					if _, ok := app.Registry.Skills[sid]; !ok {
						app.Registry.Mu.RUnlock()
						return nil, fmt.Errorf("register_role: skill %q not found. Please use orchestrator_invoke(subsystem=\"config\", action=\"register_skill\", args=...) to create it first", sid)
					}
				}
				app.Registry.Mu.RUnlock()
			}

			ops := []config.PatchOp{
				{Op: config.OpAdd, Path: "/roles/" + id, Value: json.RawMessage(data)},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true, "id": id}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["unregister_role"] = Action{
		Name:        "unregister_role",
		Description: "Remove a registered role. Persists to config.json.",
		Parameters: map[string]any{
			"id": "string (required) - role identifier to remove",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			if id == "" {
				return nil, fmt.Errorf("unregister_role: 'id' is required and must not be empty")
			}
			ops := []config.PatchOp{
				{Op: config.OpRemove, Path: "/roles/" + id},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true, "id": id}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["register_skill"] = Action{
		Name:        "register_skill",
		Description: "Register or update a skill. Persists to workspace/skills/<id>.json.",
		Parameters: map[string]any{
			"id":          "string (required) - unique skill identifier",
			"name":        "string (required)",
			"capability":  "string (required)",
			"description": "string",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			data, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("register_skill: failed to serialize args: %w", err)
			}
			var sk config.Skill
			if err := json.Unmarshal(data, &sk); err != nil {
				return nil, fmt.Errorf("register_skill: failed to parse skill fields: %w", err)
			}
			if sk.ID == "" {
				return nil, fmt.Errorf("register_skill: 'id' is required and must not be empty")
			}

			// Determine the correct skills directory
			skillsDir := app.Registry.GetConfigPath()
			skillsDir = filepath.Join(config.ConfigDir(skillsDir), "workspace/skills/")
			if err := os.MkdirAll(skillsDir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create skills dir: %w", err)
			}

			// Save skill to individual JSON file, sanitize ID to prevent path traversal
			safeID := filepath.Base(sk.ID)
			skillPath := filepath.Join(skillsDir, safeID+".json")
			skillData, err := json.MarshalIndent(sk, "", "  ")
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(skillPath, skillData, 0644); err != nil {
				return nil, err
			}

			// Update in-memory registry
			app.Registry.Mu.Lock()
			app.Registry.Skills[sk.ID] = &sk
			app.Registry.Mu.Unlock()

			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["reload"] = Action{
		Name:        "reload",
		Description: "Reload configuration from disk and clear provider caches.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			providers.ClearCache()
			if err := app.Registry.Load(); err != nil {
				return nil, fmt.Errorf("failed to reload registry: %w", err)
			}
			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["query_groups"] = Action{
		Name:        "query_groups",
		Description: "List all registered RoleGroups with their members and policy.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			app.Registry.Mu.RLock()
			defer app.Registry.Mu.RUnlock()
			var groups []config.RoleGroup
			for _, g := range app.Registry.RoleGroups {
				if g != nil {
					groups = append(groups, *g)
				}
			}
			return groups, nil
		},
	}

	s.Actions["register_group"] = Action{
		Name:        "register_group",
		Description: "Register or update a RoleGroup. Persists to config.json.",
		Parameters: map[string]any{
			"id":                 "string (required) - unique group identifier",
			"name":               "string (required)",
			"description":        "string",
			"policy":             "string (required) - sequential|parallel|voting|chain_of_thought|sop",
			"members":            "[]object (required) - [{role_id, task_template, provider, weight, optional}]",
			"aggregator_role_id": "string (optional) - role to merge parallel/voting results",
			"max_parallelism":    "int (optional) - max concurrent steps (0=unlimited)",
			"sop_ref":            "string (optional) - SOP ID for sop policy",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			if id == "" {
				return nil, fmt.Errorf("register_group: 'id' is required")
			}
			data, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("register_group: failed to serialize args: %w", err)
			}

			// FIX (2026-08-31): validate that every member's role_id references
			// an already-registered role before committing -- same rationale as
			// the matching register_role fix above (patch.go/CommitOps has no
			// knowledge of cross-referential domain constraints).
			var membersCheck struct {
				Members []struct {
					RoleID string `json:"role_id"`
				} `json:"members"`
			}
			if jerr := json.Unmarshal(data, &membersCheck); jerr == nil {
				app.Registry.Mu.RLock()
				for _, m := range membersCheck.Members {
					if m.RoleID == "" {
						continue
					}
					if _, ok := app.Registry.Roles[m.RoleID]; !ok {
						app.Registry.Mu.RUnlock()
						return nil, fmt.Errorf("register_group: role %q not found. Register the role first", m.RoleID)
					}
				}
				app.Registry.Mu.RUnlock()
			}

			ops := []config.PatchOp{
				{Op: config.OpAdd, Path: "/role_groups/" + id, Value: json.RawMessage(data)},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true, "id": id}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["unregister_group"] = Action{
		Name:        "unregister_group",
		Description: "Remove a registered RoleGroup. Persists to config.json.",
		Parameters:  map[string]any{"id": "string (required) - group identifier to remove"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			if id == "" {
				return nil, fmt.Errorf("unregister_group: 'id' is required and must not be empty")
			}
			ops := []config.PatchOp{
				{Op: config.OpRemove, Path: "/role_groups/" + id},
			}
			actor := strArg(args, "_actor")
			if actor == "" {
				actor = "admin"
			}
			taskID := strArg(args, "_task_id")
			return map[string]any{"ok": true, "id": id}, app.Registry.CommitOps(ops, actor, taskID)
		},
	}

	s.Actions["dispatch_group"] = Action{
		Name:        "dispatch_group",
		Description: "Expand a RoleGroup into a TaskGraph and submit it. Returns task_id. Weak router models can call this without knowing internal step structure.",
		Parameters: map[string]any{
			"group_id":     "string (required) - ID of the RoleGroup to dispatch",
			"task":         "string (required) - the user task description",
			"context_refs": "object (optional) - key->value references passed to all steps",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			groupID := strArg(args, "group_id")
			task := strArg(args, "task")
			if groupID == "" {
				return nil, fmt.Errorf("dispatch_group: 'group_id' is required")
			}
			if task == "" {
				return nil, fmt.Errorf("dispatch_group: 'task' is required")
			}
			ctxRefs := map[string]string{}
			if raw, ok := args["context_refs"]; ok && raw != nil {
				if m, ok := raw.(map[string]any); ok {
					for k, v := range m {
						if sv, ok := v.(string); ok {
							ctxRefs[k] = sv
						}
					}
				}
			}
			dispatcher := core.NewGroupDispatcher(app.Scheduler, app.Registry)
			taskID, err := dispatcher.DispatchGroup(groupID, task, ctxRefs, nil, nil, nil, "")
			if err != nil {
				return nil, err
			}
			return map[string]any{"task_id": taskID, "group_id": groupID}, nil
		},
	}
	registry.Register(s)
}

func registerExperienceSubsystem(app *App) {
	s := &Subsystem{
		Name:        "exp",
		Description: "Historical patterns and success rates.",
		Actions:     make(map[string]Action),
	}

	s.Actions["get_brief"] = Action{
		Name:        "get_brief",
		Description: "Get orchestration brief for specific capabilities.",
		Parameters:  map[string]any{"capabilities": "string array"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			caps := strSliceArg(args, "capabilities")
			return app.ExpStore.GetOrchestrationBrief(ctx, caps), nil
		},
	}

	s.Actions["get_summary"] = Action{
		Name:        "get_summary",
		Description: "High-level summary of experience store.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			profiles := app.ExpStore.GetRoleProfilesSnapshot()
			topRoles := make([]map[string]any, 0)
			for _, p := range profiles {
				if p.TotalRuns < 2 {
					continue
				}
				topRoles = append(topRoles, map[string]any{
					"role_id": p.RoleID, "total_runs": p.TotalRuns, "success_rate": float64(p.SuccessCount) / float64(p.TotalRuns),
				})
			}
			return map[string]any{
				"task_patterns": len(app.ExpStore.GetTaskPatternsSnapshot()),
				"top_roles":     topRoles,
			}, nil
		},
	}

	s.Actions["get_roi_summary"] = Action{
		Name:        "get_roi_summary",
		Description: "Get efficiency and ROI metrics based on historical execution.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			// GetTaskPatternsSnapshot handles its own locking internally.
			patterns := app.ExpStore.GetTaskPatternsSnapshot()
			totalTasks := 0
			totalSteps := 0
			confSum := 0.0
			for _, p := range patterns {
				totalTasks += p.SampleCount
				totalSteps += p.SampleCount * len(p.StepSequence)
				confSum += p.AvgConfidence * float64(p.SampleCount)
			}

			avgConf := 0.0
			if totalTasks > 0 {
				avgConf = confSum / float64(totalTasks)
			}

			// Efficiency calculation: tokens saved estimate (planning avoidance)
			// Each repeated pattern saves a planning call (~1500 tokens)
			tokensSaved := 0
			for _, p := range patterns {
				if p.SampleCount > 1 {
					tokensSaved += (p.SampleCount - 1) * 1500
				}
			}

			return map[string]any{
				"total_tasks_orchestrated":   totalTasks,
				"total_steps_executed":       totalSteps,
				"estimated_tokens_saved":     tokensSaved,
				"estimated_time_saved_secs":  totalTasks * 30, // 30s per task orchestrated
				"avg_confidence_vs_baseline": fmt.Sprintf("%.2f", avgConf),
				"efficiency_score":           fmt.Sprintf("%.2f", 0.8+avgConf*0.1),
			}, nil
		},
	}

	registry.Register(s)
}

func registerScheduleSubsystem(app *App) {
	s := &Subsystem{
		Name:        "schedule",
		Description: "Task scheduling and automation.",
		Actions:     make(map[string]Action),
	}

	s.Actions["list"] = Action{
		Name:        "list",
		Description: "List all schedules.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return app.SStore.List(), nil
		},
	}

	s.Actions["create"] = Action{
		Name:        "create",
		Description: "Create a new schedule for a task.",
		Parameters: map[string]any{
			"name":        "string (required)",
			"type":        "string (required) - once|interval|cron",
			"run_at":      "string (optional) - ISO8601 for 'once'",
			"interval":    "string (optional) - duration like '1h', '30m' for 'interval'",
			"cron_expr":   "string (optional) - cron expression for 'cron'",
			"task_inputs": "[]object (required) - step definitions",
			"max_runs":    "int (optional)",
			"on_failure":  "string (optional) - ignore|retry_next_window|alert",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			data, err := json.Marshal(args)
			if err != nil {
				return nil, err
			}
			var req struct {
				Name       string                  `json:"name"`
				Type       schemas.ScheduleType    `json:"type"`
				RunAt      *time.Time              `json:"run_at"`
				Interval   string                  `json:"interval"`
				CronExpr   string                  `json:"cron_expr"`
				TaskInputs []schemas.StepInput     `json:"task_inputs"`
				MaxRuns    int                     `json:"max_runs"`
				OnFailure  schemas.OnFailurePolicy `json:"on_failure"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return nil, err
			}

			if req.Name == "" || req.Type == "" || len(req.TaskInputs) == 0 {
				return nil, fmt.Errorf("missing name, type, or task_inputs")
			}

			sch := &schemas.Schedule{
				ID:         fmt.Sprintf("sch_%s", uuid.New().String()[:6]),
				Name:       req.Name,
				Type:       req.Type,
				RunAt:      req.RunAt,
				Interval:   req.Interval,
				CronExpr:   req.CronExpr,
				TaskInputs: req.TaskInputs,
				Status:     schemas.ScheduleActive,
				MaxRuns:    req.MaxRuns,
				OnFailure:  req.OnFailure,
				CreatedAt:  time.Now(),
			}

			// Initial NextRun calculation is handled by ScheduleManager.scan
			// but we can set it now to be safe.
			// We can't easily call ScheduleManager.calculateNextRun here due to circular deps
			// (App -> ScheduleManager -> Scheduler -> App).
			// Instead, we just set it to Now to trigger it on the next scan, or let scan handle it.
			sch.NextRun = time.Now()
			if sch.Type == schemas.ScheduleOnce && sch.RunAt != nil {
				sch.NextRun = *sch.RunAt
			}

			app.SStore.Set(sch)
			return map[string]any{"ok": true, "schedule_id": sch.ID}, nil
		},
	}

	s.Actions["pause"] = Action{
		Name:        "pause",
		Description: "Pause a schedule.",
		Parameters:  map[string]any{"schedule_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "schedule_id")
			sch := app.SStore.Get(id)
			if sch == nil {
				return nil, fmt.Errorf("not found")
			}
			sch.Status = schemas.SchedulePaused
			app.SStore.Set(sch)
			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["delete"] = Action{
		Name:        "delete",
		Description: "Delete a schedule.",
		Parameters:  map[string]any{"schedule_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "schedule_id")
			app.SStore.Mu.Lock()
			delete(app.SStore.Schedules, id)
			app.SStore.Mu.Unlock()
			app.SStore.Save()
			return map[string]any{"ok": true}, nil
		},
	}

	s.Actions["reload"] = Action{
		Name:        "reload",
		Description: "Reload configuration from disk and clear provider caches.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			providers.ClearCache()
			if err := app.Registry.Load(); err != nil {
				return nil, fmt.Errorf("failed to reload registry: %w", err)
			}
			return map[string]any{"ok": true}, nil
		},
	}

	registry.Register(s)
}

func registerAdminSubsystem(app *App) {
	s := &Subsystem{
		Name:        "admin",
		Description: "Logs, stats, and task management.",
		Actions:     make(map[string]Action),
	}

	s.Actions["list_tasks"] = Action{
		Name:        "list_tasks",
		Description: "List task graphs.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return app.Scheduler.ListGraphs(), nil
		},
	}

	s.Actions["clear_task"] = Action{
		Name:        "clear_task",
		Description: "Clear a completed task.",
		Parameters:  map[string]any{"task_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "task_id")
			return map[string]any{"ok": true}, app.Scheduler.Clear(id)
		},
	}

	s.Actions["get_stats"] = Action{
		Name:        "get_stats",
		Description: "Get usage statistics.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return core.GetGlobalStats().GetSnapshot(), nil
		},
	}

	s.Actions["get_manifest"] = Action{
		Name:        "get_manifest",
		Description: "Get full task manifest.",
		Parameters:  map[string]any{"task_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "task_id")
			manifest, ok := app.Scheduler.GetManifest(id)
			if !ok {
				return nil, fmt.Errorf("not found")
			}
			return manifest, nil
		},
	}

	s.Actions["get_step_result"] = Action{
		Name:        "get_step_result",
		Description: "Get result of a specific task step.",
		Parameters:  map[string]any{"task_id": "string", "step_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			tid := strArg(args, "task_id")
			sid := strArg(args, "step_id")
			res := app.Scheduler.GetStepResult(tid, sid)
			if res == nil {
				return nil, fmt.Errorf("not found")
			}
			return res, nil
		},
	}

	s.Actions["patch_workspace"] = Action{
		Name:        "patch_workspace",
		Description: "Inject or modify structured data in a task's GlobalWorkspace (EvoX). Allows external correction of data flow.",
		Parameters: map[string]any{
			"task_id": "string (required)",
			"data":    "object (required) - Key-value pairs to merge into workspace",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			tid := strArg(args, "task_id")
			data, ok := args["data"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("'data' must be an object")
			}
			if tid == "" {
				return nil, fmt.Errorf("'task_id' is required")
			}
			return map[string]any{"ok": true}, app.Scheduler.PatchWorkspace(tid, data)
		},
	}

	s.Actions["get_coordination_report"] = Action{
		Name:        "get_coordination_report",
		Description: "Calculate coordination metrics (arXiv:2608.16801) for a task graph: network density, centrality, and S/M token ratio.",
		Parameters: map[string]any{
			"task_id": "string (required)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			tid := strArg(args, "task_id")
			manifest, ok := app.Scheduler.GetManifest(tid)
			if !ok {
				return nil, fmt.Errorf("task %s not found", tid)
			}

			// In a real implementation, we'd deserialize to TaskGraph to access CoordinationEdges.
			// Since GetManifest returns map[string]any, we look for coordination_edges key.
			edgesRaw, ok := manifest["coordination_edges"].([]any)
			if !ok || len(edgesRaw) == 0 {
				return map[string]any{"status": "no_data", "message": "No coordination edges recorded for this task."}, nil
			}

			totalPayload := int64(0)
			sharedPayload := int64(0)
			messagePayload := int64(0)
			nodeMap := make(map[string]bool)

			for _, e := range edgesRaw {
				edge := e.(map[string]any)
				payload := int64(edge["payload"].(float64))
				totalPayload += payload
				if edge["type"] == "shared_vfs" {
					sharedPayload += payload
				} else {
					messagePayload += payload
				}
				nodeMap[edge["source"].(string)] = true
				nodeMap[edge["target"].(string)] = true
			}

			numNodes := len(nodeMap)
			numEdges := len(edgesRaw)
			density := 0.0
			if numNodes > 1 {
				density = float64(numEdges) / float64(numNodes*(numNodes-1))
			}

			return map[string]any{
				"task_id":          tid,
				"network_density":  fmt.Sprintf("%.4f", density),
				"node_count":       numNodes,
				"edge_count":       numEdges,
				"total_data_load":  totalPayload,
				"sm_ratio":         fmt.Sprintf("%.2f", float64(sharedPayload)/float64(messagePayload+1)), // Avoid div by zero
				"shared_data_pct":  fmt.Sprintf("%.1f%%", float64(sharedPayload)/float64(totalPayload+1)*100),
				"message_data_pct": fmt.Sprintf("%.1f%%", float64(messagePayload)/float64(totalPayload+1)*100),
			}, nil
		},
	}

	s.Actions["import_skills"] = Action{
		Name:        "import_skills",
		Description: "Import external agentskills.io-compliant SKILL.md bundles into the Vortex registry. Scans a directory recursively for SKILL.md files and registers them as native Skills (and SOPs if multi-step).",
		Parameters: map[string]any{
			"skill_directory": "string (required) - Absolute path to root directory containing SKILL.md files",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			skillDir := strArg(args, "skill_directory")
			if skillDir == "" {
				return nil, fmt.Errorf("skill_directory is required")
			}
			adapter := core.NewExternalSkillAdapter(app.Registry)
			count, err := adapter.ImportSkillDirectory(skillDir)
			if err != nil {
				return nil, fmt.Errorf("import failed: %w", err)
			}
			return map[string]any{
				"imported_count": count,
				"directory":      skillDir,
			}, nil
		},
	}

	s.Actions["debug_dump"] = Action{
		Name:        "debug_dump",
		Description: "Get a goroutine stack dump for debugging hangs.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			buf := make([]byte, 1024*1024)
			n := runtime.Stack(buf, true)
			return map[string]string{"stack": string(buf[:n])}, nil
		},
	}

	registry.Register(s)
}

func registerJitSubsystem(app *App) {
	s := &Subsystem{
		Name:        "jit",
		Description: "Dynamic JIT tools and script providers.",
		Actions:     make(map[string]Action),
	}

	s.Actions["create"] = Action{
		Name:        "create",
		Description: "Create a temporary JIT tool. Sandboxed by default (256MB memory limit, 30s CPU limit). Embedded langs (js, lua) run in-process via goja/gopher-lua — no host runtime required. Managed langs (python, node, bun, lua-via-host) spawn a sandboxed host process.",
		Parameters: map[string]any{
			"script":    "string (required)",
			"language":  "string (required) - js|lua|python|node|bun (js/lua use embedded engine; python/node/bun use host binary)",
			"ttl":       fmt.Sprintf("number (optional, default %d)", app.Registry.System.DefaultJITTTL),
			"sandboxed": "boolean (optional, default true)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			script := strArg(args, "script")
			lang := strArg(args, "language")
			ttl := intArg(args, "ttl", app.Registry.System.DefaultJITTTL)

			sandboxed := true
			if app.Tier == "public" {
				sandboxed = true
			} else if val, ok := args["sandboxed"].(bool); ok {
				sandboxed = val
			}

			// Preflight gate (architecture §二.2): reject bad code before any
			// file I/O or process spawn. RegisterTool re-runs this, but doing
			// it here too gives the action layer a chance to surface the
			// structured PreflightResult to the caller (e.g. which backend
			// was chosen) without a second round-trip.
			pf := core.PreflightCheckDetailed(lang, script)
			if !pf.OK {
				return map[string]any{"ok": false, "preflight": pf}, pf.Err
			}

			id, err := app.JIT.RegisterTool(script, lang, time.Duration(ttl)*time.Second, sandboxed)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"mcp_id":   id,
				"lang":     pf.Lang,
				"embedded": pf.Capability.IsEmbedded,
				"backend":  string(pf.Capability.Type),
			}, nil
		},
	}

	s.Actions["list_runtimes"] = Action{
		Name:        "list_runtimes",
		Description: "List all JIT runtime backends (embedded + managed) and their availability. Embedded backends are always available; managed backends reflect the current host.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return core.ListRuntimeCapabilities(), nil
		},
	}

	s.Actions["list_scripts"] = Action{
		Name:        "list_scripts",
		Description: "List all script-based providers.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return providers.GetScriptProviders(), nil
		},
	}

	s.Actions["list_active"] = Action{
		Name:        "list_active",
		Description: "List all active temporary JIT tools.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			app.Registry.Mu.RLock()
			defer app.Registry.Mu.RUnlock()
			var result []map[string]any
			for id, mcp := range app.Registry.DynamicMCPs {
				result = append(result, map[string]any{
					"id":         id,
					"expires_at": mcp.ExpiresAt.Format(time.RFC3339),
					"command":    mcp.Command,
				})
			}
			return result, nil
		},
	}

	// eval: POC (playbook/jit-session-poc-design-2026.md) session-scoped JIT
	// interpreter, distinct from "create" above -- create spawns a new,
	// stateless, one-shot tool per call; eval reuses one persistent,
	// sandboxed interpreter across multiple calls scoped to (task_id,
	// step_id), so variables/imports genuinely survive between calls.
	//
	// As of the embedded-runtime + preflight architecture
	// (docs/architecture/EMBEDDED_JIT_AND_PREFLIGHT_DESIGN.md), eval also
	// serves as the inline execution path for embedded langs (js, lua):
	// those run in-process via goja/gopher-lua with NO session state (a
	// fresh VM per call), since the embedded engines are cheap to construct
	// and the safety lint guarantees no host escape. python still uses the
	// persistent subprocess session.
	s.Actions["eval"] = Action{
		Name: "eval",
		Description: "Evaluate code in a sandboxed JIT context. For lang=\"python\", " +
			"uses a persistent session scoped to (task_id, step_id) so variables/imports " +
			"survive across calls within the same step. For lang=\"js\" or \"lua\", runs " +
			"in-process via the embedded goja/gopher-lua engine (no host runtime required, " +
			"fresh VM per call). All langs pass through the preflight gate (syntax + safety " +
			"lint + runtime availability) before execution.",
		Parameters: map[string]any{
			"task_id":     "string (required)",
			"step_id":     "string (required)",
			"code":        "string (required)",
			"lang":        "string (optional, default \"python\") - python|js|lua",
			"ttl_seconds": "number (optional, default 300)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			taskID := strArg(args, "task_id")
			stepID := strArg(args, "step_id")
			code := strArg(args, "code")
			if taskID == "" || stepID == "" || code == "" {
				return nil, fmt.Errorf("jit.eval: task_id, step_id, and code are all required")
			}
			lang := strArg(args, "lang")
			if lang == "" {
				lang = "python"
			}
			ttl := intArg(args, "ttl_seconds", 300)

			// Preflight gate (architecture §二.2): runs before any session
			// spawn or embedded VM construction. Returns a structured result
			// so the caller can see which backend was chosen.
			pf := core.PreflightCheckDetailed(lang, code)
			if !pf.OK {
				return map[string]any{"ok": false, "preflight": pf}, pf.Err
			}

			// Embedded tier: js/lua run in-process, no session state.
			if pf.Capability.IsEmbedded {
				res, err := core.EmbeddedRunner(ctx, pf.Lang, code)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"stdout":   res.Stdout,
					"stderr":   res.Stderr,
					"result":   res.Result,
					"embedded": true,
					"backend":  string(pf.Capability.Type),
				}, nil
			}

			// Managed tier: python uses the persistent session.
			if app.Scheduler == nil || app.Scheduler.JITSessions == nil {
				return nil, fmt.Errorf("jit.eval: JIT session manager not initialized (required for lang=%q)", pf.Lang)
			}
			sess, err := app.Scheduler.JITSessions.GetOrCreateSessionForStep(taskID, stepID, pf.Lang, time.Duration(ttl)*time.Second)
			if err != nil {
				return nil, err
			}
			res, err := sess.Eval(ctx, code)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"stdout":   res.Stdout,
				"stderr":   res.Stderr,
				"result":   res.Result,
				"embedded": false,
				"backend":  string(pf.Capability.Type),
			}, nil
		},
	}

	registry.Register(s)
}

// registerCapabilitySubsystem wires the Unified Capability Driver actions.
// See docs/architecture/UNIFIED_CAPABILITY_DISCOVERY_DESIGN.md.
func registerCapabilitySubsystem(app *App) {
	s := &Subsystem{
		Name:        "capability",
		Description: "Dynamic capability discovery and lifecycle (mount/unmount/inspect) via the unified driver pattern.",
		Actions:     make(map[string]Action),
	}

	s.Actions["mount"] = Action{
		Name:        "mount",
		Description: "Inspect then mount a remote capability (SSE endpoint) into the registry. The capability becomes immediately available to all roles.",
		Parameters: map[string]any{
			"id":           "string (required) - capability identifier",
			"transport":    "string (optional, default \"remote_sse\") - remote_sse|local_process|sandbox_jit",
			"endpoint":     "string (required) - URL for remote_sse",
			"manifest":     "object (optional) - auth config {auth_header_name, api_key_env}",
			"skip_inspect": "boolean (optional, default false) - skip the pre-flight Inspect check",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			endpoint := strArg(args, "endpoint")
			if id == "" || endpoint == "" {
				return nil, fmt.Errorf("capability.mount: id and endpoint are required")
			}
			transport := strArg(args, "transport")
			if transport == "" {
				transport = "remote_sse"
			}
			def := config.CapabilityDef{
				ID:        id,
				Transport: config.CapabilityTransport(transport),
				Endpoint:  endpoint,
			}
			if raw, ok := args["manifest"].(map[string]any); ok {
				def.Manifest = raw
			}

			driver, err := core.NewCapabilityDriver(def, app.Registry)
			if err != nil {
				return nil, err
			}

			skipInspect, _ := args["skip_inspect"].(bool)
			if !skipInspect {
				if err := driver.Inspect(ctx); err != nil {
					return map[string]any{"ok": false, "inspect_error": err.Error()}, err
				}
			}
			if err := driver.Mount(ctx); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "id": id, "transport": transport, "endpoint": endpoint}, nil
		},
	}

	s.Actions["unmount"] = Action{
		Name:        "unmount",
		Description: "Unmount a previously mounted capability. Idempotent.",
		Parameters:  map[string]any{"id": "string (required)"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			if id == "" {
				return nil, fmt.Errorf("capability.unmount: id is required")
			}
			def := config.CapabilityDef{ID: id, Transport: config.TransportRemoteSSE}
			driver, err := core.NewCapabilityDriver(def, app.Registry)
			if err != nil {
				return nil, err
			}
			if err := driver.Unmount(ctx); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "id": id}, nil
		},
	}

	s.Actions["inspect"] = Action{
		Name:        "inspect",
		Description: "Pre-flight check whether a remote capability endpoint is reachable. Does not mount.",
		Parameters: map[string]any{
			"id":       "string (required)",
			"endpoint": "string (required) - URL to check",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "id")
			endpoint := strArg(args, "endpoint")
			if id == "" || endpoint == "" {
				return nil, fmt.Errorf("capability.inspect: id and endpoint are required")
			}
			def := config.CapabilityDef{ID: id, Transport: config.TransportRemoteSSE, Endpoint: endpoint}
			driver, err := core.NewCapabilityDriver(def, app.Registry)
			if err != nil {
				return nil, err
			}
			if err := driver.Inspect(ctx); err != nil {
				return map[string]any{"ok": false, "error": err.Error()}, err
			}
			return map[string]any{"ok": true, "id": id, "endpoint": endpoint}, nil
		},
	}

	registry.Register(s)
}

func registerProxySubsystem(app *App) {
	s := &Subsystem{
		Name:        "proxy",
		Description: "Direct tool execution for high-performance session agents. Vortex acts as a resource gateway.",
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

			bindings, err := core.ResolveFinalMCPs(hub, roleID, nil, nil)
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

func registerIntelSubsystem(app *App) {
	s := &Subsystem{
		Name:        "intel",
		Description: "Deep code analysis.",
		Actions:     make(map[string]Action),
	}

	for _, t := range ci.AllTools() {
		name := t.Name
		s.Actions[name] = Action{
			Name:        name,
			Description: t.Description,
			Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
				res := app.CodeIntel.Dispatch(name, args)
				if res.IsError {
					if len(res.Content) > 0 {
						return nil, fmt.Errorf("%s", res.Content[0].Text)
					}
					return nil, fmt.Errorf("unknown error")
				}
				if len(res.Content) > 0 {
					return res.Content[0].Text, nil
				}
				return "OK", nil
			},
		}
	}

	s.Actions["get_code_details"] = Action{
		Name:        "get_code_details",
		Description: "Retrieves the full implementation details of a compressed code file or a specific symbol (function/method).",
		Parameters: map[string]any{
			"path":   "string (required) - Absolute path to the source file",
			"symbol": "string (optional) - Name of the function or method to retrieve; if omitted, returns the whole file",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			path := strArg(args, "path")
			symbol := strArg(args, "symbol")
			if path == "" {
				return nil, fmt.Errorf("path is required")
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("failed to read file: %w", err)
			}

			src := string(data)
			if !strings.HasSuffix(path, ".go") {
				return src, nil
			}

			comp := codeintel.NewCompressor(0)
			details, err := comp.GetDetails(path, src, symbol)
			if err != nil {
				return nil, err
			}
			return details, nil
		},
	}

	registry.Register(s)
}

func registerModelsSubsystem(app *App) {
	s := &Subsystem{
		Name:        "models",
		Description: "Available AI models and capabilities for dynamic escalation.",
		Actions:     make(map[string]Action),
	}

	s.Actions["list"] = Action{
		Name:        "list",
		Description: "List all active providers and their capabilities.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			if app.Registry == nil {
				return nil, fmt.Errorf("registry not available")
			}
			return app.Registry.GetHealthyModelsSummary(), nil
		},
	}

	registry.Register(s)
}
