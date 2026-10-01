package tools

import (
	"context"
)

// ConfigSubsystem implements SubsystemHandler for the "config" namespace.
// It delegates to the existing handler functions registered in the legacy
// subsystem registry, providing modular dispatch without code duplication.
//
// This is the first migrated subsystem (design §4 Incremental Migration Guide).
// Handler functions remain in subsystems.go's registerConfigSubsystem; they
// will be extracted to this file incrementally in follow-up commits.
type ConfigSubsystem struct{}

func (s *ConfigSubsystem) SubsystemName() string { return "config" }

func (s *ConfigSubsystem) HandleAction(ctx context.Context, app *App, action string, args map[string]any) (any, error) {
	// Delegate to the legacy registry which handles permission checks,
	// timeout, logging, and usage tracking. The config subsystem's handlers
	// are still registered there; this wrapper provides modular namespace
	// routing. Handlers will be extracted to this file incrementally.
	return registry.Invoke(ctx, app, "config", action, args)
}

func init() {
	RegisterSubsystem(&ConfigSubsystem{})
}
