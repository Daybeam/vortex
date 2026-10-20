package core

import (
	"sort"

	"github.com/daybeam/vortex/config"
)

// stickyUnionBindings adds previously-called tools back to the routed
// bindings so that re-routing does not drop tools the model was actively
// using (R2 fix).
//
// Without this, a tool called in turn N (e.g. show_sent_payment_requests)
// can disappear from the bindings in turn N+1 because the task text changed
// and the re-route no longer matches it. This breaks multi-turn workflows
// and invalidates prompt-cache prefixes.
//
// Security boundary: the original authorization set from initialBindings,
// NOT the MCP's full AvailableTools. This ensures:
//  1. Restricted bindings never widen beyond their original allowlist.
//  2. Unrestricted bindings (empty original AllowedTools = "all tools")
//     are skipped — adding tools would turn them into restricted bindings.
func (s *Spawner) stickyUnionBindings(bindings []config.MCPBinding, initialBindings []config.MCPBinding, calledTools map[string]bool) []config.MCPBinding {
	if len(calledTools) == 0 {
		return bindings
	}

	// Build MCPID → original authorization set from initialBindings.
	// Only restricted bindings (non-empty AllowedTools) participate.
	// For duplicate MCPIDs, the union of allowlists is used — this is
	// conservative but never widens beyond what was originally authorized.
	type origAuth struct {
		restricted bool
		allow      map[string]bool
	}
	orig := make(map[string]*origAuth)
	for _, b := range initialBindings {
		if len(b.AllowedTools) == 0 {
			continue // unrestricted — skip
		}
		entry := orig[b.MCPID]
		if entry == nil {
			entry = &origAuth{restricted: true, allow: make(map[string]bool)}
			orig[b.MCPID] = entry
		}
		for _, t := range b.AllowedTools {
			entry.allow[t] = true
		}
	}

	// Sort calledTools keys for deterministic AllowedTools order.
	// Map iteration in Go is randomized — without sorting, the order of
	// appended tools varies per call, which invalidates prompt-cache prefixes
	// (C17 fix, 2026-10-20).
	sortedCalled := make([]string, 0, len(calledTools))
	for tool := range calledTools {
		sortedCalled = append(sortedCalled, tool)
	}
	sort.Strings(sortedCalled)

	for i := range bindings {
		entry := orig[bindings[i].MCPID]
		if entry == nil || !entry.restricted {
			continue // unrestricted or unknown — skip
		}
		seen := make(map[string]bool, len(bindings[i].AllowedTools))
		for _, t := range bindings[i].AllowedTools {
			seen[t] = true
		}
		for _, tool := range sortedCalled {
			if entry.allow[tool] && !seen[tool] {
				bindings[i].AllowedTools = append(bindings[i].AllowedTools, tool)
				seen[tool] = true
			}
		}
	}
	return bindings
}
