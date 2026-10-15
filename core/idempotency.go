package core

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/daybeam/vortex/schemas"
)

// ─── Effect Boundary Defense: A1 Idempotency ─────────────────────────────
//
// Design ref: docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.2
//
// TurnIdempotencyContext tracks executed tool calls within a single turn
// to prevent duplicate execution (A1 anomaly defense). When a provider
// returns duplicate tool calls in the same response, only the first is
// executed; subsequent duplicates are skipped.
//
// This is complementary to the existing per-step dedup (#11 eval §8.41)
// which allows 2 identical calls before blocking. A1 is stricter: within
// a single turn, duplicate calls are always skipped.
//
// The opID reuses the existing toolCallKey (FNV-1a hash of toolName+args),
// avoiding a separate SHA256 computation.

// TurnIdempotencyContext tracks executed tool calls for one turn.
type TurnIdempotencyContext struct {
	mu       sync.Mutex
	executed map[string]bool // toolCallKey → executed
}

// NewTurnIdempotencyContext creates a fresh context for a new turn.
func NewTurnIdempotencyContext() *TurnIdempotencyContext {
	return &TurnIdempotencyContext{executed: make(map[string]bool)}
}

// HasExecuted checks if a tool call with the given key has been executed
// in this turn.
func (c *TurnIdempotencyContext) HasExecuted(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executed[key]
}

// MarkExecuted records a tool call as executed for this turn.
func (c *TurnIdempotencyContext) MarkExecuted(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executed[key] = true
}

// CheckAndMark is a combined check-and-mark: if the key has already been
// executed, returns true (skip). Otherwise marks it as executed and
// returns false (proceed). This is the common usage pattern.
func (c *TurnIdempotencyContext) CheckAndMark(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.executed[key] {
		return true // already executed — skip
	}
	c.executed[key] = true
	return false // first execution — proceed
}

// ─── Effect Boundary Defense: A5 Dry-Run Pre-Check ──────────────────────
//
// Design ref: docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.4
//
// For tools with RiskLevel=high, attempt a dry_run before real execution.
// If the tool supports dry_run (has it in InputSchema), execute with
// dry_run=true and return the preview result. If not supported, log a
// warning — the tool proceeds normally but the risk is flagged.

// supportsDryRun checks if a tool's InputSchema includes a dry_run parameter.
func supportsDryRun(toolDef *schemas.ToolDefinition) bool {
	if toolDef == nil || toolDef.InputSchema == nil {
		return false
	}
	props, ok := toolDef.InputSchema["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, hasDryRun := props["dry_run"]
	return hasDryRun
}

// isHighRiskTool checks if a tool has RiskLevel=high.
func isHighRiskTool(toolDef *schemas.ToolDefinition) bool {
	if toolDef == nil {
		return false
	}
	return toolDef.RiskLevel == "high"
}

// findCoreToolDef looks up a core tool's definition by name.
func findCoreToolDef(name string) *schemas.ToolDefinition {
	for _, def := range CoreToolDefinitions() {
		if def.Name == name {
			return &def
		}
	}
	return nil
}

// addDryRunArg returns a copy of args with dry_run=true added.
func addDryRunArg(args map[string]any) map[string]any {
	copy := make(map[string]any, len(args)+1)
	for k, v := range args {
		copy[k] = v
	}
	copy["dry_run"] = true
	return copy
}

// dryRunResult formats a dry-run result message for the model.
func dryRunResult(toolName string, result string) string {
	return fmt.Sprintf("[DRY-RUN PREVIEW] %s\n%s\n\nNote: This was a dry-run. The operation was NOT actually executed. Call again without dry_run to execute.", toolName, result)
}

// marshalArgsForLog safely marshals tool arguments for logging.
func marshalArgsForLog(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}
