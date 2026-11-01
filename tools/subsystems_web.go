//go:build web

package tools

import (
	"github.com/mark3labs/mcp-go/server"
	"github.com/daybeam/vortex/core"
)

type intentRouterSubsystem struct{}

func (s *intentRouterSubsystem) Name() string { return "intent_router" }
func (s *intentRouterSubsystem) Init(app *App) error {
	hub := core.NewContextHub(app.Registry, nil, nil)
	embedClient := core.NewHubEmbeddingClient(hub)
	intentRouter := core.NewIntentRouter(embedClient)
	intentRouter.RefreshEmbeddingsAsync(app.Registry)
	app.IntentRouter = intentRouter
	return nil
}

// registerEditionSubsystems registers subsystems specific to the web edition.
func registerEditionSubsystems(app *App, _ *server.MCPServer) {
	app.RegisterEditionSubsystem(&intentRouterSubsystem{})
}
