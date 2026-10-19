package core

import "github.com/daybeam/vortex/config"

// stickyUnionBindings adds previously-called tools back to the routed
// bindings so that re-routing does not drop tools the model was actively
// using (R2 fix).
//
// Without this, a tool called in turn N (e.g. show_sent_payment_requests)
// can disappear from the bindings in turn N+1 because the task text changed
// and the re-route no longer matches it. This breaks multi-turn workflows
// and invalidates prompt-cache prefixes.
//
// Safety: only tools that are in the MCP's AvailableTools are added back —
// this never widens access beyond what the MCP actually exposes.
func (s *Spawner) stickyUnionBindings(bindings []config.MCPBinding, calledTools map[string]bool) []config.MCPBinding {
	if len(calledTools) == 0 {
		return bindings
	}
	for i := range bindings {
		mcpDef := s.registry.GetMCP(bindings[i].MCPID)
		if mcpDef == nil {
			continue
		}
		available := make(map[string]bool, len(mcpDef.AvailableTools))
		for _, t := range mcpDef.AvailableTools {
			available[t] = true
		}
		seen := make(map[string]bool, len(bindings[i].AllowedTools))
		for _, t := range bindings[i].AllowedTools {
			seen[t] = true
		}
		for tool := range calledTools {
			if available[tool] && !seen[tool] {
				bindings[i].AllowedTools = append(bindings[i].AllowedTools, tool)
				seen[tool] = true
			}
		}
	}
	return bindings
}
