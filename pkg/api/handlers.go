package api

import (
	"github.com/daybeam/vortex/pkg/bootmetrics"
	"github.com/daybeam/vortex/tools"
)

// APIHandler wraps the App and provides HTTP handlers for the configuration API.
//
// Methods are split across domain-specific files:
//   - handlers_config.go   — config, roles, skills, MCPs, providers, groups, SOPs, system
//   - handlers_swarm.go    — swarm sensing, claim, yield
//   - handlers_system.go   — memory bank, user profile, notifications, events stream
//   - handlers_schedule.go — schedule CRUD
//   - handlers_import.go   — skill/MCP bulk import
//   - handlers_task.go     — task submit, cancel, resume, suspend, decision
//   - handlers_analyze.go  — analyze, context profile, usage
type APIHandler struct {
	App *tools.App
}

func NewAPIHandler(app *tools.App) *APIHandler {
	bootmetrics.Mark("api")
	return &APIHandler{App: app}
}
