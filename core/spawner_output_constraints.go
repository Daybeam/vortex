package core

import "github.com/daybeam/vortex/config"

// resolveOutputConstraints decides whether this turn forces a tool call and
// builds the provider-facing constraint map for it.
//
// Extracted from doSpawn's provider-candidate loop (size gate); behaviour is
// identical — forceTool is turn-0-ish pressure only (any turn before the last
// with no tool call yet, unless DisableForceTool), and the constraint field is
// provider-specific via the constraint adapter.
//
// ── Why the pressure is this narrow (do NOT widen it) ──────────────────────
// FIX (2026-09-07): Revert ForceToolCall to only turn-0 pressure.
// ForceToolCall=ANY on Gemini is a hard constraint that forbids text output —
// forcing it across the whole budget would trap the model into calling tools it
// doesn't need (like `think` with no args) just to satisfy the schema. The real
// fix is the re-prompt nudge on the no-tool-call path: when the model writes
// text early, tell it to keep calling tools. This preserves the model's ability
// to decide when it's done.
//
// The "re-prompt nudge" that this comment refers to now lives in the completion
// gates (spawner_completion_gates.go + SpawnerZeroEffectMsg /
// SpawnerMidAbandonMsg in prompt_constants.go), which grew out of the single
// nudge the 2026-09-07 fix introduced.
//
// Kept verbatim through the extraction: the narrow condition above is the whole
// point of the fix, and a future reader who only sees `turn < maxTurns-1` will be
// tempted to simplify it into "force tools until done". That is exactly the
// regression 2026-09-07 reverted.
func (s *Spawner) resolveOutputConstraints(pCfg *config.ProviderConfig, turn, maxTurns, traceLen int) (bool, map[string]any) {
	constraints := make(map[string]any)
	forceTool := turn < maxTurns-1 && traceLen == 0 && !s.registry.System.FeatureFlags.DisableForceTool
	if !forceTool {
		return false, constraints
	}
	toolCallSchema := map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string"},
				"arguments": map[string]any{"type": "object"},
			},
			"required": []string{"name", "arguments"},
		},
	}
	field, val := s.constraintAdapter.Resolve(pCfg.Provider, toolCallSchema)
	if field != "" {
		constraints[field] = val
	}
	return true, constraints
}
