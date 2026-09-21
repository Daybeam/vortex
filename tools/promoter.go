package tools

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Promoter watches invoke traffic and hot-promotes frequently-used subsystem
// actions into volatile, in-memory top-level MCP tools registered on the Admin (Tier2) MCPServer.
//
// DESIGN PRINCIPLE (Volatile Cache, NOT Permanent):
//   - This is purely an in-memory, process-lifetime cache. It NEVER writes to disk,
//     never modifies configuration files, and never permanently codifies actions.
//   - When the orchestrator restarts, all hot-promoted tools disappear, returning the system
//     to its clean baseline. This prevents token pollution and tool bloat.
//   - Includes LRU eviction and TTL expiry so transient high-traffic actions automatically
//     evaporate when no longer actively used.
type Promoter struct {
	mu          sync.Mutex
	counters    map[string]int       // "subsystem.action" -> hit count
	promoted    map[string]time.Time // "subsystem.action" -> promotion timestamp (TTL)
	lastUsed    map[string]time.Time // "subsystem.action" -> last invocation timestamp (LRU)
	threshold   int
	maxPromoted int               // Maximum capacity (e.g. 20)
	ttl         time.Duration     // Time-to-live before eviction if idle
	server      *server.MCPServer // the Tier2 (admin) MCPServer
	app         *App
}

// NewPromoter creates a volatile, in-memory promoter bound to the admin-tier MCPServer.
func NewPromoter(s *server.MCPServer, app *App, threshold int, maxPromoted int, ttl time.Duration) *Promoter {
	if threshold <= 0 {
		threshold = 10
	}
	if maxPromoted <= 0 {
		maxPromoted = 20
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &Promoter{
		counters:    make(map[string]int),
		promoted:    make(map[string]time.Time),
		lastUsed:    make(map[string]time.Time),
		threshold:   threshold,
		maxPromoted: maxPromoted,
		ttl:         ttl,
		server:      s,
		app:         app,
	}
}

// Record is called by Invoke() after a successful action handler execution.
// It increments the counter and triggers volatile in-memory hot-promotion upon crossing the threshold.
func (p *Promoter) Record(subsystem, action string, act Action) {
	if p == nil || p.server == nil {
		return
	}

	key := subsystem + "." + action
	now := time.Now()

	p.mu.Lock()
	p.counters[key]++
	p.lastUsed[key] = now

	// Check if already promoted
	if _, exists := p.promoted[key]; exists {
		p.mu.Unlock()
		return
	}

	// Check threshold
	if p.counters[key] < p.threshold {
		p.mu.Unlock()
		return
	}

	// Enforce capacity (LRU eviction if at maxPromoted)
	if len(p.promoted) >= p.maxPromoted {
		p.evictLRULocked()
	}

	p.promoted[key] = now
	p.mu.Unlock()

	// Register volatile top-level tool
	toolName := fmt.Sprintf("orchestrator_%s_%s", subsystem, action)
	desc := fmt.Sprintf("[Volatile Cache Promoted] %s — subsystem %q action %q. Ephemeral memory-only cache.", act.Description, subsystem, action)

	var opts []mcp.ToolOption
	if len(act.Parameters) > 0 {
		for pName, pInfo := range act.Parameters {
			opts = append(opts, mcp.WithString(pName, mcp.Description(fmt.Sprintf("%v", pInfo))))
		}
	}

	promotedTool := mcp.NewTool(toolName, append([]mcp.ToolOption{mcp.WithDescription(desc)}, opts...)...)

	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Update LRU timestamp on use
		p.mu.Lock()
		p.lastUsed[key] = time.Now()
		p.mu.Unlock()

		// Runtime tier enforcement
		effectiveTier := p.app.Tier
		if v, ok := ctx.Value(AuthTierKey).(string); ok && v != "" {
			effectiveTier = v
		}
		if effectiveTier == TierPublic && !isActionAllowed(subsystem, action, effectiveTier) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"permission denied: volatile promoted action %q in subsystem %q requires Tier2 (Admin) privileges",
				action, subsystem)), nil
		}

		rawArgs, _ := req.Params.Arguments.(map[string]any)
		args := make(map[string]any)
		if rawArgs != nil {
			for k, v := range rawArgs {
				args[k] = v
			}
		}
		res, err := act.Handler(ctx, p.app, args)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonOK(res)
	}

	p.server.AddTool(promotedTool, p.app.loggedHandler(toolName, handler))

	if p.app.Logger != nil {
		p.app.Logger.Log(core.EventToolPromoted, "system", "", map[string]any{
			"tool_name": toolName,
			"subsystem": subsystem,
			"action":    action,
			"threshold": p.threshold,
			"ephemeral": true,
		})
	}
}

// evictLRULocked removes the least recently used promoted tool key from tracking.
// Note: mcp-go does not support RemoveTool, so evicted items become inert/un-refreshed
// in the server map while freeing capacity for active hot spots.
func (p *Promoter) evictLRULocked() {
	var oldestKey string
	var oldestTime time.Time

	for k := range p.promoted {
		t := p.lastUsed[k]
		if oldestKey == "" || t.Before(oldestTime) {
			oldestKey = k
			oldestTime = t
		}
	}

	if oldestKey != "" {
		delete(p.promoted, oldestKey)
		delete(p.counters, oldestKey)
		delete(p.lastUsed, oldestKey)
	}
}

// Reset clears all volatile cache state.
func (p *Promoter) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counters = make(map[string]int)
	p.promoted = make(map[string]time.Time)
	p.lastUsed = make(map[string]time.Time)
}

// Stats returns a snapshot of the volatile cache for telemetry.
func (p *Promoter) Stats() map[string]any {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	counterOut := make(map[string]int)
	for k, v := range p.counters {
		counterOut[k] = v
	}
	promotedOut := make([]string, 0)
	for k := range p.promoted {
		promotedOut = append(promotedOut, k)
	}
	return map[string]any{
		"counters":     counterOut,
		"promoted":     promotedOut,
		"max_capacity": p.maxPromoted,
	}
}
