package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/providers"
)

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
			if EffectiveTier(ctx, app) == TierPublic {
				sandboxed = true
			} else if val, ok := args["sandboxed"].(bool); ok {
				sandboxed = val
			}

			// Preflight gate (architecture §2.2): reject bad code before any
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

	s.Actions["reload_script"] = Action{
		Name:        "reload_script",
		Description: "Hot-reload a script-based provider by name. Closes the old VM and re-reads the script file.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			name := strArg(args, "name")
			if name == "" {
				return nil, fmt.Errorf("name is required")
			}
			app.Registry.Mu.RLock()
			cfg, ok := app.Registry.Providers[name]
			app.Registry.Mu.RUnlock()
			if !ok {
				return nil, fmt.Errorf("provider %q not found in registry", name)
			}
			if err := providers.ReloadScriptProvider(name, cfg); err != nil {
				return nil, err
			}
			return map[string]any{"status": "reloaded", "name": name}, nil
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

			// Preflight gate (architecture §2.2): runs before any session
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
