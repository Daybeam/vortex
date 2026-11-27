package core

import (
	"encoding/json"
	"sync"
)

// ─── Effect Boundary Defense: A1 Idempotency ─────────────────────────────
//
// Design ref: docs/completed/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.2
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

// marshalArgsForLog safely marshals tool arguments for logging.
func marshalArgsForLog(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}
