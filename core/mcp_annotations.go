package core

import (
	"github.com/daybeam/vortex/schemas"
)

func parseToolAnnotations(t map[string]any) *schemas.ToolAnnotations {
	raw, ok := t["annotations"]
	if !ok || raw == nil {
		return nil
	}
	a, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	ann := &schemas.ToolAnnotations{}
	if v, ok := a["title"].(string); ok {
		ann.Title = v
	}
	ann.ReadOnlyHint, _ = a["readOnlyHint"].(bool)
	ann.DestructiveHint, _ = a["destructiveHint"].(bool)
	ann.IdempotentHint, _ = a["idempotentHint"].(bool)
	ann.OpenWorldHint, _ = a["openWorldHint"].(bool)
	return ann
}
