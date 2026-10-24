package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestStablePromptTime_IsTurnStable is the regression test for the KV-cache
// prefix-stability fix (2026-10-23). The time value renders into the cacheable
// system-prompt prefix (prompts/base_role.md: "Current Time: ..."), and the
// system prompt is rebuilt on every turn (spawner_dospawn.go buildSystemPrompt
// inside the turn loop). A per-call time.Now() therefore changed the prefix on
// every turn, so provider prefix caching could never hit — the engine's own
// CacheSentinel names this exact cause. The value must now be identical across
// turns (derived from the task graph's CreatedAt).
func TestStablePromptTime_IsTurnStable(t *testing.T) {
	created := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	hub := &ContextHub{Graph: &schemas.TaskGraph{TaskID: "t", CreatedAt: created}}

	a := stablePromptTime(hub)
	b := stablePromptTime(hub)
	if !a.Equal(created) {
		t.Fatalf("expected the task's CreatedAt, got %v", a)
	}
	if !a.Equal(b) {
		t.Fatalf("prompt time must be stable across turns, got %v then %v", a, b)
	}

	// No graph -> falls back to now (unchanged behavior for graph-less builds).
	fc := stablePromptTime(&ContextHub{})
	if time.Since(fc) > time.Minute {
		t.Fatalf("fallback should be approximately now, got %v", fc)
	}
}
