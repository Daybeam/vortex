package tools

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// RegisterFastPathSubsystem exposes the FastPath engine as a subsystem.
func RegisterFastPathSubsystem(registry *SubsystemRegistry, app *App) {
	s := &Subsystem{
		Name:        "fastpath",
		Description: "Deterministic execution and contract validation without LLM overhead.",
		Actions:     make(map[string]Action),
	}

	s.Actions["execute_contract"] = Action{
		Name:        "execute_contract",
		Description: "Execute a typed artifact contract via deterministic validator locally.",
		Parameters: map[string]any{
			"path":            "string (required) - Path to the artifact file",
			"required_fields": "array (optional) - List of required JSON fields",
			"type_schema":     "object (optional) - Map of field names to expected JSON types",
			"workdir":         "string (optional) - Workspace root for relative paths",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			path := strArg(args, "path")
			if path == "" {
				return nil, fmt.Errorf("fastpath.execute_contract: 'path' is required")
			}

			workdir := strArg(args, "workdir")
			if workdir == "" {
				// Fallback to a safe default if no registry available,
				// though RegisterAll always provides one.
				workdir = "."
			}

			contract := schemas.ArtifactContract{
				Path: path,
			}

			if rf, ok := args["required_fields"].([]any); ok {
				for _, f := range rf {
					if s, ok := f.(string); ok {
						contract.RequiredFields = append(contract.RequiredFields, s)
					}
				}
			}

			if ts, ok := args["type_schema"].(map[string]any); ok {
				contract.TypeSchema = make(map[string]string)
				for k, v := range ts {
					if s, ok := v.(string); ok {
						contract.TypeSchema[k] = s
					}
				}
			}

			engine := core.NewFastPathEngine(workdir)
			return engine.ExecuteFastPath(ctx, contract, args)
		},
	}

	registry.Register(s)
}
