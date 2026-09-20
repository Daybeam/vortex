package core

import (
	"context"
	"fmt"
	"os"
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

func (s *Spawner) buildMCPServers(
	ctx context.Context,
	hub *ContextHub,
	taskID string,
	bindings []config.MCPBinding,
	providerName string,
) []providers.MCPServerDef {
	var servers []providers.MCPServerDef

	// Inject core tools (write_file, read_file, execute_code) as a virtual MCP server
	servers = append(servers, providers.MCPServerDef{
		Name:  "_core",
		URL:   "core://internal",
		Tools: CoreToolDefinitions(),
	})

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
			cli, err := s.mcpMgr.ensureMCPClient(ctx, mcp, taskID)
			if err != nil {
				// FIX (2026-08-30): Log the error and continue with cached tool
				// definitions if available. Previously, a failed MCP client spawn
				// silently skipped the entire MCP binding, causing mcp_servers_count
				// to be 0 even when FullToolDefinitions were already cached from
				// the workspace JSON config. This made dynamically-mounted tools
				// invisible to the model (e.g., book_researcher + leann-mcp).
				s.logger.Log("EventMCPClientFailed", taskID, "", map[string]any{
					"mcp":          mcp.ID,
					"error":        err.Error(),
					"cached_tools": len(mcp.FullToolDefinitions),
				})
				if len(mcp.FullToolDefinitions) == 0 {
					dLock.Unlock()
					continue // No cached tools and no client — truly unusable
				}
			}

			// Tool Discovery: Fetch full schemas if not already cached
			if cli != nil && len(mcp.FullToolDefinitions) == 0 {
				discCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				// audit M7: don't defer cancel() — it accumulates in this for loop.
				// Call cancel() explicitly at the end of this block instead.
				// FIX (2026-08-15): Swapped order to prioritize modern standard (tools/list)
				// over legacy (list_tools). The handshake above in ensureMCPClient
				// already resolved the -32602 issue for uninitialized calls.
				var res any
				var err error
				res, err = cli.SendRequest(discCtx, "tools/list", map[string]any{})
				if err != nil {
					// Fallback to legacy standard
					res, err = cli.SendRequest(discCtx, "list_tools", map[string]any{})
				}

				if err == nil {
					if m, ok := res.(map[string]any); ok {
						if toolsRaw, ok := m["tools"].([]any); ok {
							s.registry.Mu.Lock()
							for _, tRaw := range toolsRaw {
								if t, ok := tRaw.(map[string]any); ok {
									name, _ := t["name"].(string)
									desc, _ := t["description"].(string)
									def := schemas.ToolDefinition{
										Name:        name,
										Description: desc,
									}
									if is, ok := t["inputSchema"].(map[string]any); ok {
										def.InputSchema = is
									}
									mcp.FullToolDefinitions = append(mcp.FullToolDefinitions, def)
								}
							}
							s.registry.Mu.Unlock()
							s.logger.Log("EventMCPToolsDiscovered", taskID, "", map[string]any{
								"mcp":   mcp.ID,
								"count": len(mcp.FullToolDefinitions),
							})
						}
					}
				} else {
					s.logger.Log("EventMCPToolsDiscoveryFailed", taskID, "", map[string]any{
						"mcp":   mcp.ID,
						"error": err.Error(),
					})
				}
				cancel() // audit M7: explicit cancel instead of defer (avoids accumulation in for loop)
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
