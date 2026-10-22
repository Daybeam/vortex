package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// Regression for A4 (eval FINDINGS.md §A4, 2026-10-22):
// The write-auth "advisory" path (require_write_reason=false → the call is
// nudged but allowed to proceed) logged EventWriteAuthBlocked even though
// nothing was blocked. In the v20 run this produced 15 "Blocked" + 15 "Nudged"
// pairs ~30µs apart and misled diagnosis into believing every write had been
// vetoed. After the fix, EventWriteAuthBlocked fires only on the real
// hard-block path (require_write_reason=true), where the call is actually
// refused.

// writeAuthTurnEvents runs one LLM-initiated write call through
// processToolCalls and returns the set of logger event names it emitted.
func writeAuthTurnEvents(t *testing.T, requireWriteReason bool) map[string]bool {
	t.Helper()

	reg := &config.Registry{
		Roles:  map[string]*config.Role{},
		Skills: map[string]*config.Skill{},
		MCPs:   map[string]*config.MCPDef{},
		System: config.SystemSettings{RequireWriteReason: requireWriteReason},
	}
	logger := mustNewLogger(t, mustTempDir(t, "write-auth"), &config.SystemSettings{})
	sub := logger.SubscribeEvents()

	s := &Spawner{registry: reg, logger: logger, processor: NewOutputProcessor(3000)}
	hub := NewContextHub(reg, nil, nil)
	req := &SpawnRequest{TaskID: "task-writeauth", StepID: "s1", RoleID: "role", Hub: hub}
	// A write-verb tool with no _reason → reason defaults to LLMInitiated.
	resp := &providers.ProviderResponse{ToolCalls: []schemas.ToolCall{
		{Name: "cancel_reservation", Arguments: map[string]any{"reservation_id": "R-1"}},
	}}
	state := &toolExecState{
		toolCallCount:  map[string]int{},
		toolFailCount:  map[string]int{},
		resultStreak:   map[string]int{},
		resultLastHash: map[string]uint64{},
	}

	s.processToolCalls(context.Background(), req, resp, nil, "", NewTurnIdempotencyContext(), state)

	// Close flushes the writer goroutine, so every event has been broadcast to
	// the subscription channel before we drain it.
	logger.Close()
	seen := map[string]bool{}
	for {
		select {
		case ev := <-sub:
			seen[string(ev.Event)] = true
		default:
			return seen
		}
	}
}

func TestWriteAuthEvents_AdvisoryPathIsNotBlocked(t *testing.T) {
	seen := writeAuthTurnEvents(t, false)
	if seen["EventWriteAuthBlocked"] {
		t.Error("advisory path (require_write_reason=false) must NOT emit EventWriteAuthBlocked — the call proceeds")
	}
	if !seen["EventWriteAuthNudged"] {
		t.Error("advisory path must emit EventWriteAuthNudged")
	}
}

func TestWriteAuthEvents_HardBlockPathIsBlocked(t *testing.T) {
	seen := writeAuthTurnEvents(t, true)
	if !seen["EventWriteAuthBlocked"] {
		t.Error("hard-block path (require_write_reason=true) must emit EventWriteAuthBlocked")
	}
	if seen["EventWriteAuthNudged"] {
		t.Error("hard-block path must NOT emit EventWriteAuthNudged — the call is refused, not nudged")
	}
}