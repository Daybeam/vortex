package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/daybeam/vortex/pkg/codeintel"
	ci "github.com/daybeam/vortex/pkg/codeintel/tools"
)

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
