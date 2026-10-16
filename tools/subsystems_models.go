package tools

import (
	"context"
	"fmt"
)

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
