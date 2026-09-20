package tools

import (
	"fmt"

	"github.com/mark3labs/mcp-go/server"
)

// EditionSubsystem represents an optional, edition-specific component that
// can be registered and initialized through a unified lifecycle. This
// eliminates scattered per-edition wiring in main_*.go and the associated
// "forgot to wire in one edition → nil panic" risk.
type EditionSubsystem interface {
	Name() string
	Init(app *App) error
}

// RegisterEditionSubsystem adds a subsystem to the app's registry.
func (app *App) RegisterEditionSubsystem(s EditionSubsystem) {
	app.editionSubsystems = append(app.editionSubsystems, s)
}

// InitEditionSubsystems runs Init on all registered subsystems in order.
func (app *App) InitEditionSubsystems() error {
	for _, s := range app.editionSubsystems {
		if err := s.Init(app); err != nil {
			return fmt.Errorf("subsystem %s: %w", s.Name(), err)
		}
	}
	return nil
}

// SetupSubsystems is the single entry point for edition-specific subsystem
// wiring. It delegates to registerEditionSubsystems (build-tag-gated) then
// runs Init on all registered subsystems. Each main_*.go calls this instead
// of scattered field assignments.
func (app *App) SetupSubsystems(mcpServer *server.MCPServer) error {
	RegisterFastPathSubsystem(registry, app)
	registerEditionSubsystems(app, mcpServer)
	return app.InitEditionSubsystems()
}
