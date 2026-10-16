package tools

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
)

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
