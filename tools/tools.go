package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	ci "github.com/daybeam/vortex/pkg/codeintel/tools"
	"github.com/daybeam/vortex/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// authTierKeyType is the unexported context key type for the caller's auth tier.
type authTierKeyType struct{}

// AuthTierKey is the context key used to propagate the caller's auth tier.
var AuthTierKey = authTierKeyType{}

// authTokenKeyType is the unexported context key type for the actual token used.
type authTokenKeyType struct{}

// AuthTokenKey is the context key used to propagate the actual token used for auth.
var AuthTokenKey = authTokenKeyType{}

const (
	// TierAdmin grants access to all tools including privileged subsystems.
	TierAdmin = "admin"
	// TierPublic grants access to safe read/submit tools only.
	TierPublic = "public"
)

// App holds all dependencies injected into tool handlers.
type App struct {
	Registry          *config.Registry
	Scheduler         *core.DirectedEngine
	TaskStore         store.ITaskStore
	ExpStore          store.IExperienceStore
	Schedule          *core.ScheduleManager
	SStore            *store.ScheduleStore
	JIT               *core.JITManager
	Logger            *core.Logger
	CodeIntel         *ci.Handler
	Navigator         *core.CapabilityNavigator
	ConfigPath        string
	Tier              string
	ResourceLoader    *core.ResourceLoader
	IntentRouter      *core.IntentRouter
	EmbedClient       core.EmbeddingClient
	MemoryBankStore   *store.MemoryBankStore
	Archive           *core.ContextArchive
	Promoter          *Promoter // Optional usage-driven hot-promoter for invoke actions
	SOPManager        *core.SOPManager
	DB                *sql.DB
	editionSubsystems []EditionSubsystem
	archiveStop       chan struct{} // stops the TTL pruning goroutine (audit: was goroutine leak)
}

// InitArchive wires the ContextArchive (hot-data memory) into the app and
// scheduler, and starts a background TTL pruning goroutine. Called from
// each main_*.go after app creation. Single-task scope only — the
// orchestrator_context_search tool forces task_scope to the caller's task.
func (app *App) InitArchive(outputBase string) {
	app.Archive = core.NewContextArchive(filepath.Join(outputBase, "memory", "context.jsonl"))
	if app.Scheduler != nil {
		app.Scheduler.Archive = app.Archive
	}
	app.archiveStop = make(chan struct{})
	stop := app.archiveStop
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if pruned, err := app.Archive.PruneExpired(time.Now()); err == nil && pruned > 0 {
					if app.Logger != nil {
						app.Logger.Log("EventPruned", "", "", map[string]any{"count": pruned})
					}
				}
			}
		}
	}()
}

// StopArchive stops the background TTL pruning goroutine (audit: was goroutine leak).
func (app *App) StopArchive() {
	if app.archiveStop != nil {
		close(app.archiveStop)
		app.archiveStop = nil
	}
}

// jsonOK marshals v and returns a text result.
func jsonOK(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// loggedHandler wraps an MCP tool handler with structured logging for observability.
func (app *App) loggedHandler(name string, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if app.Logger == nil {
			return handler(ctx, req)
		}

		start := time.Now()
		taskID := strArg(req.Params.Arguments, "task_id")

		effectiveTier := app.Tier
		if v, ok := ctx.Value(AuthTierKey).(string); ok {
			effectiveTier = v
		}

		app.Logger.Log(core.EventToolCall, taskID, "", map[string]any{
			"tool":  name,
			"args":  req.Params.Arguments,
			"tier":  effectiveTier,
			"phase": "started",
		})

		res, err := handler(ctx, req)

		duration := time.Since(start).Seconds()
		detail := map[string]any{
			"tool":     name,
			"duration": duration,
			"phase":    "finished",
			"tier":     effectiveTier,
		}

		if err != nil {
			detail["error"] = err.Error()
		} else if res != nil && res.IsError {
			detail["error"] = "tool execution failed"
		}

		app.Logger.Log(core.EventToolCall, taskID, "", detail)
		return res, err
	}
}

// register adds a tool to s and wraps its handler with logging.
func (app *App) register(s *server.MCPServer, tool mcp.Tool, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)) {
	s.AddTool(tool, app.loggedHandler(tool.Name, handler))
}

func errResult(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(msg), nil
}

func strArg(rawArgs any, key string) string {
	args, _ := rawArgs.(map[string]any)
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

func strSliceArg(rawArgs any, key string) []string {
	args, _ := rawArgs.(map[string]any)
	if args == nil {
		return nil
	}
	raw, ok := args[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return splitQuotedCSV(v)
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func intArg(rawArgs any, key string, def int) int {
	args, _ := rawArgs.(map[string]any)
	if args == nil {
		return def
	}
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

func int64Arg(rawArgs any, key string, def int64) int64 {
	args, _ := rawArgs.(map[string]any)
	if args == nil {
		return def
	}
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return def
}

// RegisterAll registers all tools on s (full admin-tier access).
func RegisterAll(s *server.MCPServer, app *App) {
	RegisterTier1(s, app)
	RegisterTier2(s, app)
}

// RegisterTier1 registers public-safe tools.
func RegisterTier1(s *server.MCPServer, app *App) {
	RegisterSubsystems(app)
	registerTaskList(s, app)
}

// RegisterTier2 registers admin-only tools.
func RegisterTier2(s *server.MCPServer, app *App) {
	registerAdminTools(s, app)
	registerCommandList(s, app)
}

func splitQuotedCSV(s string) []string {
	var result []string
	var current strings.Builder
	inDoubleQuote := false
	inSingleQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
			current.WriteByte(c)
		case c == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
			current.WriteByte(c)
		case c == ',' && !inDoubleQuote && !inSingleQuote:
			result = append(result, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	result = append(result, strings.TrimSpace(current.String()))
	return result
}
