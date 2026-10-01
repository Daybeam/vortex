package tools

import (
	"context"
)

// SubsystemHandler defines the standard interface for modular subsystems
// behind orchestrator_invoke.
type SubsystemHandler interface {
	// SubsystemName returns the subsystem identifier (e.g. "config", "group", "admin")
	SubsystemName() string

	// HandleAction processes a specific action within this subsystem.
	HandleAction(ctx context.Context, app *App, action string, args map[string]any) (any, error)
}
