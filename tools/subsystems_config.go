package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

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
		Description: "Deprecated: use discover(scope=pool, type=roles) for lightweight summaries. Returns full role objects for backward compatibility.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			// Phase 4 (2026-10-20): NOT redirected to DiscoverImpl because the
			// legacy format returns full config.Role objects (Instruction,
			// BoundSkills, BoundMCPBindings, etc.) while DiscoverImpl returns
			// lightweight DiscoveryRoleSummary. Converting back would require
			// a registry lookup per role — no benefit over direct read.
			// New callers should use discover(scope=pool, type=roles).
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
			if EffectiveTier(ctx, app) == TierPublic {
				return nil, fmt.Errorf("permission denied: public tier cannot modify global workspace assets (update_sop)")
			}
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
			if EffectiveTier(ctx, app) == TierPublic {
				return nil, fmt.Errorf("permission denied: public tier cannot modify global workspace assets (register_role)")
			}
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
			if EffectiveTier(ctx, app) == TierPublic {
				return nil, fmt.Errorf("permission denied: public tier cannot modify global workspace assets (register_skill)")
			}
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
			taskID, err := dispatcher.DispatchGroup(groupID, task, ctxRefs, nil, nil, nil, "", EffectiveTier(ctx, app) == TierAdmin)
			if err != nil {
				return nil, err
			}
			return map[string]any{"task_id": taskID, "group_id": groupID}, nil
		},
	}
	registry.Register(s)
}
