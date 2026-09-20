//go:build !web && !dev && !desktop

package tools

import (
	"time"

	"github.com/mark3labs/mcp-go/server"
)

type promoterSubsystem struct {
	mcpServer *server.MCPServer
}

func (s *promoterSubsystem) Name() string { return "promoter" }
func (s *promoterSubsystem) Init(app *App) error {
	app.Promoter = NewPromoter(s.mcpServer, app, 10, 20, 2*time.Hour)
	return nil
}

// registerEditionSubsystems registers subsystems specific to the standard edition.
func registerEditionSubsystems(app *App, mcpServer *server.MCPServer) {
	app.RegisterEditionSubsystem(&promoterSubsystem{mcpServer: mcpServer})
}
