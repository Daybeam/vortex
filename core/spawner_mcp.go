package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// ResolveToolMCP finds which MCP server owns a given tool by checking available_tools.
//
// FIX (2026-09-07): Tool-name collision routing.
//
// Before this fix the fallback was "return bindings[0].MCPID". With a role like
// ghost_operator bound to [agentdesk-browser, chrome-devtools, ghost-driver], a
// tool that no binding explicitly declares — e.g. ghost-driver's connect_browser,
// whose available_tools list was not yet populated — was silently routed to
// bindings[0] (agentdesk-browser, a dead Node binary). The call then failed with
// "Module not found browser-connector.js", and the LLM correctly reported
// capability_required, so the step was marked failed even though a healthy MCP
// (ghost-driver, 48 tools) was bound alongside it.
//
// Root cause was two-layered:
//  1. The name-based scan could not match an unpopulated available_tools list.
//  2. The position-based fallback picked the first binding regardless of health.
//
// Both layers are now fixed: a name match in declared tools still wins, then we
// fall back to the first binding that is actually healthy (has discovered tools
// AND is not marked as a dead handshake). Only if nothing is healthy do we return
// "" so the caller can produce a real error instead of a misrouted one.
func (s *Spawner) ResolveToolMCP(toolName string, bindings []config.MCPBinding, hub *ContextHub) string {
	if len(bindings) == 0 {
		return ""
	}

	// Tier 1: tool explicitly declared by a binding's available_tools.
	for _, b := range bindings {
		mcp := hub.GetMCP(b.MCPID)
		if mcp == nil {
			continue
		}
		if mcp.ToolFilter != nil && !mcp.ToolFilter.Test(toolName) {
			continue
		}
		if containsStr(mcp.AvailableTools, toolName) {
			return b.MCPID
		}
	}

	// Tier 2: tool declared in a restricted binding's explicit allow-list.
	for _, b := range bindings {
		if b.IsRestricted() && containsStr(b.AllowedTools, toolName) {
			return b.MCPID
		}
	}

	// Tier 3: name unknown — route to a binding that is actually healthy.
	// Prefer one whose tools were discovered over one that is merely declared.
	best := ""
	bestScore := -1
	for _, b := range bindings {
		mcp := hub.GetMCP(b.MCPID)
		if mcp == nil {
			continue
		}
		score := 0
		if len(mcp.AvailableTools) > 0 {
			score++
		}
		// FullToolDefinitions is populated only after a successful handshake,
		// so it is a stronger health signal than the static available_tools list.
		// Lock per-MCP to avoid racing with concurrent discovery (audit H8).
		if s != nil && s.mcpMgr != nil {
			dLock := s.mcpMgr.discoveryLock(mcp.ID)
			dLock.Lock()
			ftdLen := len(mcp.FullToolDefinitions)
			dLock.Unlock()
			if ftdLen > 0 {
				score++
			}
		} else if len(mcp.FullToolDefinitions) > 0 {
			score++
		}
		if s != nil && s.mcpMgr != nil && s.mcpMgr.hasMCPSpawnFailed(mcp.ID) {
			score -= 10
		}
		if score > bestScore {
			bestScore = score
			best = b.MCPID
		}
	}
	if bestScore >= 1 {
		return best
	}

	// Nothing healthy and nothing declared. Returning "" lets the caller report a
	// truthful error ("MCP server not found for tool X") instead of misrouting
	// the call into a dead server whose error message masks the real cause.
	return ""
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// unknownToolFeedback reports a call whose name the target MCP does not serve,
// with near-miss suggestions. Returns "" when the call should be forwarded.
//
// Tier 3 of ResolveToolMCP deliberately routes a name it does not recognise to
// a healthy binding: for a tool that is merely *undeclared* but real, that is
// the right answer. For a tool that does not exist, it is the wrong answer — the
// call is forwarded, the server replies "Unknown tool", and the model receives
// an empty body with no way to tell a phantom from a transient failure.
//
// Measured 2026-10-19 (AppWorld 4fab96f_3, run u): the role instruction lists
// all nine apps it *can* operate ("supervisor, spotify, venmo, amazon, todoist,
// gmail, phone, simple_note, splitwise"), while that run's MCP binding exposed
// only supervisor+venmo. The agent called splitwise__login, gmail__login and
// simple_note__login — five calls, all empty responses. The AppWorld call log
// records them as ok=false chars=0, which is what a nonexistent tool looks like
// from the outside.
//
// Guarded on a populated catalog: an empty AvailableTools means discovery has
// not run (cold start), and every name would look unknown.
func (s *Spawner) unknownToolFeedback(toolName, mcpID string, mcp *config.MCPDef) string {
	if mcpID == "" || mcp == nil || len(mcp.AvailableTools) == 0 {
		return ""
	}
	if containsStr(mcp.AvailableTools, toolName) {
		return ""
	}
	// A filter may legitimately hide tools that discovery did report; a name
	// accepted by the filter but missing from the cached list is a cache gap,
	// not a phantom. Forward those.
	if mcp.ToolFilter != nil && mcp.ToolFilter.Test(toolName) {
		return ""
	}

	near := nearMissTools(toolName, mcp.AvailableTools, 3)
	var hint string
	if len(near) > 0 {
		hint = " The closest tools this session does have are: " + strings.Join(near, ", ") + ". Use one of them if it does what you need."
	} else {
		hint = " Use only tools from the list you were given."
	}
	return fmt.Sprintf(
		"[%s] No such tool in this session: the %q server exposes %d tools and none is named %q.%s "+
			"Calling it again cannot succeed.",
		toolName, mcpID, len(mcp.AvailableTools), toolName, hint)
}

// nearMissTools ranks catalog entries by shared namespace (the "app__" prefix)
// then by shared verb tokens, and returns up to limit names.
func nearMissTools(want string, catalog []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	wantNS, wantVerb := splitToolName(want)
	type scored struct {
		name  string
		score int
	}
	var hits []scored
	for _, cand := range catalog {
		if cand == want {
			continue
		}
		ns, verb := splitToolName(cand)
		score := 0
		if wantNS != "" && ns == wantNS {
			score += 2
		}
		if wantVerb != "" && verb != "" {
			for _, tok := range strings.Split(wantVerb, "_") {
				if len(tok) >= 3 && strings.Contains(verb, tok) {
					score++
				}
			}
		}
		if score > 0 {
			hits = append(hits, scored{cand, score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].name < hits[j].name
	})
	out := make([]string, 0, limit)
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		out = append(out, h.name)
	}
	return out
}

func splitToolName(name string) (namespace, verb string) {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[:i], name[i+2:]
	}
	return "", name
}

// discoverStdioMCP runs tools/list discovery for a single stdio MCP if not
// already cached. Returns true if the MCP is usable (has tools, discovered or
// cached); false if the client cannot be started and no tools are cached.
//
// Extracted from buildMCPServers (2026-10-19) to fix the timing race where
// Route() at spawner_prepare.go:101 read AvailableTools before tools/list
// discovery, causing cold-start tasks to route against an empty tool set.
// Now called from ensureMCPDiscovery BEFORE Route, and from buildMCPServers
// (idempotent: the len(FullToolDefinitions)==0 guard skips re-discovery).
//
// Caller MUST hold the per-MCP discoveryLock (audit H8) to serialize
// concurrent discovery + FullToolDefinitions read/write.
func (s *Spawner) discoverStdioMCP(ctx context.Context, mcp *config.MCPDef, taskID string) bool {
	cli, err := s.mcpMgr.ensureMCPClient(ctx, mcp, taskID)
	if err != nil {
		// FIX (2026-08-30): Log and continue with cached tools if available.
		s.logger.Log("EventMCPClientFailed", taskID, "", map[string]any{
			"mcp":          mcp.ID,
			"error":        err.Error(),
			"cached_tools": len(mcp.FullToolDefinitions),
		})
		return len(mcp.FullToolDefinitions) > 0
	}

	if cli == nil || len(mcp.FullToolDefinitions) > 0 {
		return true // already discovered, or no client but cached tools
	}

	discCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	// audit M7: don't defer cancel() — caller pattern expects explicit cancel.
	// FIX (2026-08-15): Prioritize tools/list (modern) over list_tools (legacy).
	res, err := cli.SendRequest(discCtx, "tools/list", map[string]any{})
	if err != nil {
		res, err = cli.SendRequest(discCtx, "list_tools", map[string]any{})
	}

	if err != nil {
		// #18 (eval §8.31): Surface at ERROR level so it's visible in standard logs.
		slog.Error("MCP tools/list discovery failed — MCP will have 0 tools",
			"mcp", mcp.ID,
			"error", err.Error(),
			"task", taskID)
		s.logger.Log("EventMCPToolsDiscoveryFailed", taskID, "", map[string]any{
			"mcp":   mcp.ID,
			"error": err.Error(),
		})
		cancel()
		return false
	}

	var toolsRaw []any
	if m, ok := res.(map[string]any); ok {
		if tr, ok := m["tools"].([]any); ok && len(tr) > 0 {
			toolsRaw = tr
		}
	}
	if toolsRaw == nil {
		cancel()
		return false
	}

	// NOTE (2026-10-18): registry unlock must happen BEFORE InitFilter/
	// RefreshToolEmbeddings (they take registry.Mu.RLock → self-deadlock).
	s.registry.Mu.Lock()
	for _, tRaw := range toolsRaw {
		t, ok := tRaw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := t["description"].(string)
		def := schemas.ToolDefinition{
			Name:        name,
			Description: desc,
		}
		if is, ok := t["inputSchema"].(map[string]any); ok {
			def.InputSchema = is
		}
		mcp.FullToolDefinitions = append(mcp.FullToolDefinitions, def)
		mcp.AvailableTools = append(mcp.AvailableTools, name)
	}
	s.registry.Mu.Unlock()

	mcp.InitFilter()
	// C1 fix (2026-10-19): nil check — toolRouter may not be initialized
	// in test contexts or early startup before NewSpawner completes wiring.
	if s.toolRouter != nil {
		s.toolRouter.RefreshToolEmbeddings(ctx, s.registry)
	}
	s.logger.Log("EventMCPToolsDiscovered", taskID, "", map[string]any{
		"mcp":   mcp.ID,
		"count": len(mcp.FullToolDefinitions),
	})
	cancel()
	return true
}

// ensureMCPDiscovery runs tools/list discovery for all stdio MCP bindings
// BEFORE Route() so that Route sees populated AvailableTools on cold start.
//
// FIX (2026-10-19): timing race. Route() at spawner_prepare.go:101 read
// AvailableTools before buildMCPServers' discovery at spawner_mcp.go:223,
// causing cold-start tasks to route against an empty tool set (only
// always_include tools survived). This restores the documented contract
// ("cache populated once during spawner initialization, reused across
// steps" per docs/architecture/architecture-mcp-gaps.md §3).
//
// Only discovers stdio MCPs (mcp.Command != ""). Remote MCP discovery
// stays in buildMCPServers (provider-dependent: skips Anthropic passthrough).
// Idempotent: discoverStdioMCP skips MCPs with FullToolDefinitions populated.
func (s *Spawner) ensureMCPDiscovery(ctx context.Context, hub *ContextHub, taskID string, bindings []config.MCPBinding) {
	for _, b := range bindings {
		mcp := hub.GetMCP(b.MCPID)
		if mcp == nil || mcp.Command == "" {
			continue // remote MCPs (mcp.URL) handled in buildMCPServers
		}
		// C1 fix (2026-10-19): don't check FullToolDefinitions here (outside
		// the lock) — concurrent discovery could be appending to it right now,
		// causing a data race. The idempotency check inside discoverStdioMCP
		// (under the lock) handles skip-already-discovered correctly.
		dLock := s.mcpMgr.discoveryLock(mcp.ID)
		dLock.Lock()
		s.discoverStdioMCP(ctx, mcp, taskID)
		dLock.Unlock()
	}
}

// ensureMCPDiscoveryForRole is the ContextHub-free form, resolved against the
// registry. Called by RequestPlanning before the planner renders its tool
// catalog: planning happens before any step exists, so without this the planner
// sees AvailableTools == nil for every MCP and can only either emit an empty
// catalog (rejected by ParsePlanDAG as "tool catalog unavailable") or invent
// tool names.
//
// The planner is supposed to be the one component that sees every capability,
// so its catalog must not depend on a step having run first. Discovery is
// cached per MCPDef and idempotent, so the first step pays nothing extra.
//
// Returns the number of MCPs whose discovery succeeded, for logging.
func (s *Spawner) ensureMCPDiscoveryForRole(ctx context.Context, roleID, taskID string) int {
	if s == nil || s.registry == nil || s.mcpMgr == nil {
		return 0
	}

	s.registry.Mu.RLock()
	var ids []string
	if r, ok := s.registry.Roles[roleID]; ok && r != nil {
		for _, b := range r.BoundMCPBindings {
			if b.MCPID != "" {
				ids = append(ids, b.MCPID)
			}
		}
	}
	if len(ids) == 0 {
		// The planner role may legitimately bind nothing; then it is planning
		// for a role whose MCPs we do not know. Fall back to every stdio MCP,
		// which is what plannerToolCatalog does for the same case.
		for id, m := range s.registry.MCPs {
			if m != nil && m.Command != "" {
				ids = append(ids, id)
			}
		}
	}
	s.registry.Mu.RUnlock()

	discovered := 0
	for _, id := range ids {
		s.registry.Mu.RLock()
		mcp := s.registry.MCPs[id]
		s.registry.Mu.RUnlock()
		if mcp == nil || mcp.Command == "" {
			continue
		}
		dLock := s.mcpMgr.discoveryLock(mcp.ID)
		dLock.Lock()
		ok := s.discoverStdioMCP(ctx, mcp, taskID)
		dLock.Unlock()
		if ok {
			discovered++
		}
	}
	return discovered
}

func (s *Spawner) buildMCPServers(
	ctx context.Context,
	hub *ContextHub,
	taskID string,
	bindings []config.MCPBinding,
	providerName string,
	roleID string,
) []providers.MCPServerDef {
	var servers []providers.MCPServerDef

	// #1 (eval §8.31): Core tools (write_file, read_file, execute_code) as a
	// virtual MCP server. Default: injected first (backward compat). Set
	// VORTEX_CORE_TOOLS_ORDER=last to inject after domain MCPs so small
	// models see domain tools first instead of biasing toward sandbox tools.
	//
	// ADDED 2026-10-19: the surface is now role-scoped. A role declaring
	// core_tools: [] (task_planner) gets no core server at all, so its prompt
	// cannot tempt it into executing the task itself. Enforcement is mirrored
	// at dispatch — see coreToolPolicy.
	coreDefs := s.coreToolPolicyFor(roleID).filter(CoreToolDefinitions())
	coreLast := s.registry.System.FeatureFlags.CoreToolsOrderLast
	var coreServer *providers.MCPServerDef
	if len(coreDefs) > 0 {
		coreServer = &providers.MCPServerDef{
			Name:  "_core",
			URL:   "core://internal",
			Tools: coreDefs,
		}
		if !coreLast {
			servers = append(servers, *coreServer)
		}
	}

	for _, b := range bindings {
		mcp := hub.GetMCP(b.MCPID)
		if mcp == nil {
			continue
		}

		// Per-MCP lock serializes access to FullToolDefinitions so concurrent
		// buildMCPServers calls for the same MCP don't race on discovery +
		// read/write of the tool-definition slice (audit H8).
		dLock := s.mcpMgr.discoveryLock(mcp.ID)
		dLock.Lock()

		if mcp.Command != "" {
			// Discovery (extracted to discoverStdioMCP, also called from
			// ensureMCPDiscovery before Route to fix the cold-start timing race).
			// Idempotent: skips if FullToolDefinitions already populated.
			if !s.discoverStdioMCP(ctx, mcp, taskID) {
				dLock.Unlock()
				continue // No cached tools and no client — truly unusable
			}

			// Filter tools based on allowed list
			var tools []schemas.ToolDefinition
			allowedMap := make(map[string]bool)
			for _, t := range b.AllowedTools {
				allowedMap[t] = true
			}

			for _, t := range mcp.FullToolDefinitions {
				if !b.IsRestricted() || allowedMap[t.Name] {
					tools = append(tools, t)
				}
			}

			servers = append(servers, providers.MCPServerDef{
				Name:         b.MCPID,
				URL:          "local://" + mcp.ID,
				AllowedTools: b.AllowedTools,
				Tools:        tools,
			})
		} else {
			// FIX (2026-07-02): URL-based (remote) MCPs previously never had tool
			// discovery attempted for non-Anthropic providers (Anthropic gets
			// discovery for free via its native mcp_servers passthrough to the
			// API; Gemini/OpenAI/Ollama build their function-declaration arrays
			// directly from MCPServerDef.Tools, which was always nil/empty for
			// these bindings -- see 06-30 addendum for the original diagnosis).
			// Do a lightweight JSON-RPC "tools/list" discovery over HTTP for
			// non-Anthropic providers, using per-MCP auth config if present.
			if mcp.URL != "" && providerName != "anthropic" && len(mcp.FullToolDefinitions) == 0 {
				discCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				tools, err := s.mcpMgr.discoverRemoteMCPTools(discCtx, mcp)
				cancel()
				if err == nil {
					mcp.FullToolDefinitions = tools
					mcp.DiscoveryStatus = config.DiscoveryOK
					s.logger.Log("EventMCPToolsDiscovered", taskID, "", map[string]any{
						"mcp":   mcp.ID,
						"count": len(tools),
					})
				} else {
					// Apply granular status based on error content
					status := config.DiscoveryFailed
					errStr := err.Error()
					if strings.Contains(errStr, "no URL") {
						status = config.DiscoveryNoURL
					} else if strings.Contains(errStr, "no api_key_env") || strings.Contains(errStr, "unset or empty") {
						status = config.DiscoveryNoAuth
					}
					mcp.DiscoveryStatus = status

					// #18 (eval §8.31): Surface remote tools/list failure at ERROR level.
					slog.Error("MCP remote tools/list discovery failed — MCP will have 0 tools",
						"mcp", mcp.ID,
						"status", status,
						"error", errStr,
						"task", taskID)
					s.logger.Log("EventMCPToolsDiscoveryFailed", taskID, "", map[string]any{
						"mcp":    mcp.ID,
						"status": status,
						"error":  errStr,
					})
				}
			} else if mcp.URL == "" {
				mcp.DiscoveryStatus = config.DiscoveryNoURL
			}

			var tools []schemas.ToolDefinition
			allowedMap := make(map[string]bool)
			for _, t := range b.AllowedTools {
				allowedMap[t] = true
			}
			for _, t := range mcp.FullToolDefinitions {
				if !b.IsRestricted() || allowedMap[t.Name] {
					tools = append(tools, t)
				}
			}

			servers = append(servers, providers.MCPServerDef{
				Name:         b.MCPID,
				URL:          mcp.URL,
				AllowedTools: b.AllowedTools,
				Tools:        tools,
			})
		}
		dLock.Unlock() // audit H8: release per-MCP discovery lock
	}

	if coreLast && coreServer != nil {
		servers = append(servers, *coreServer)
	}
	return servers
}

// DirectExecute allows Verifiers to call tools without triggering the SAV loop.
// Moved from spawner.go during god-class split.
func (s *Spawner) DirectExecute(ctx context.Context, mcpID, toolName string, args map[string]any) (any, error) {
	hub := NewContextHub(s.registry, nil, s.expStore)
	mcpDef := hub.GetMCP(mcpID)
	if mcpDef == nil {
		return nil, fmt.Errorf("MCP server %s not found", mcpID)
	}

	// Embedded JIT marker: dispatch to the in-process EmbeddedRunner instead
	// of spawning a host binary. The marker is set by JITManager.
	// RegisterEmbeddedTool; the script path is in Args[0]. This is the
	// zero-dependency execution path for js/lua JIT tools.
	if strings.HasPrefix(mcpDef.Command, EmbeddedCommandPrefix) {
		return executeEmbeddedMCP(ctx, mcpDef, toolName, args)
	}

	if mcpDef.Command != "" {
		// Validate arguments against the tool's JSON Schema before dispatching.
		if err := validateToolArgs(mcpDef, toolName, args); err != nil {
			return nil, err
		}
		cli, err := s.mcpMgr.ensureMCPClient(ctx, mcpDef, "direct_execute")
		if err != nil {
			return nil, err
		}
		return cli.SendRequest(ctx, "tools/call", map[string]any{
			"name":      toolName,
			"arguments": args,
		})
	} else if mcpDef.URL != "" {
		return s.mcpMgr.callRemoteMCPTool(ctx, mcpDef, toolName, args)
	}

	return nil, fmt.Errorf("MCP %s has neither command nor URL", mcpID)
}

// executeEmbeddedMCP reads the embedded JIT script from disk and runs it via
// EmbeddedRunner. The script is expected to define a function named toolName
// (or "run" as default) which is invoked with args. For the common JIT case
// where the script is a self-contained program, toolName is ignored and the
// whole script is executed.
// Moved from spawner.go during god-class split.
func executeEmbeddedMCP(ctx context.Context, mcpDef *config.MCPDef, toolName string, args map[string]any) (any, error) {
	lang := strings.TrimPrefix(mcpDef.Command, EmbeddedCommandPrefix)
	if len(mcpDef.Args) == 0 {
		return nil, fmt.Errorf("embedded MCP %s has no script path", mcpDef.ID)
	}
	scriptBytes, err := os.ReadFile(mcpDef.Args[0])
	if err != nil {
		return nil, fmt.Errorf("embedded MCP %s: read script: %w", mcpDef.ID, err)
	}
	res, err := EmbeddedRunner(ctx, lang, string(scriptBytes))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"stdout": res.Stdout,
		"stderr": res.Stderr,
		"result": res.Result,
	}, nil
}
