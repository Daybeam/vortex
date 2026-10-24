package core

import (
	"fmt"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// maybeAppendDisclosureHint appends the one-time Progressive-Disclosure hint on
// the first turn, when many tools were hidden from the model.
//
// Extracted from doSpawn's provider-candidate loop (size gate); no control-flow
// effect — it returns the (possibly extended) userBlocks.
func (s *Spawner) maybeAppendDisclosureHint(
	hub *ContextHub, initialBindings, bindings []config.MCPBinding,
	userBlocks []schemas.ContentBlock, turn int, progressiveEnabled bool,
) []schemas.ContentBlock {
	if !(turn == 0 && progressiveEnabled) {
		return userBlocks
	}
	totalTools := 0
	for _, b := range initialBindings {
		if m := hub.GetMCP(b.MCPID); m != nil {
			totalTools += len(m.AvailableTools)
		}
	}
	visibleTools := 0
	for _, b := range bindings {
		visibleTools += len(b.AllowedTools)
	}
	if totalTools > visibleTools+5 {
		hint := fmt.Sprintf("\n[SYSTEM HINT]: I have over %d specialized tools available but am currently exposing only the %d most relevant discovery/status tools to keep your context focused. Use discovery tools (list, search, status) to explore the environment; as your intent becomes more specific, I will automatically unlock the necessary specialized tools.", totalTools, visibleTools)
		userBlocks = append(userBlocks, schemas.ContentBlock{Text: hint, CacheControl: "ephemeral"})
	}
	return userBlocks
}
