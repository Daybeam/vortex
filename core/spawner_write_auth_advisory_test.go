package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// Regression tests for the write-auth gate (eval FINDINGS.md §A3/§A4,
// 2026-10-22).
//
// A4: the "advisory" path (require_write_reason=false → the call is nudged
// but allowed to proceed) logged EventWriteAuthBlocked even though nothing
// was blocked. In the v20 run this produced 15 "Blocked" + 15 "Nudged" pairs
// ~30µs apart and misled diagnosis into believing every write had been
// vetoed. After the fix, EventWriteAuthBlocked fires only on the real
// hard-block path (require_write_reason=true), where the call is actually
// refused.
//
// A3: on that same advisory path the nudge itself went only to state.trace
// (never model-visible — the next turn's observation is built from
// state.toolResults), so v20's 15 EventWriteAuthNudged all fired into the
// void while the model saw only the bare result. After the fix the nudge is
// appended to toolResults ahead of the real result, and one call produces
// exactly one trace entry.

// runWriteAuthTurn runs one LLM-initiated write call (no _reason, no MCP
// registered, so the call fails with "MCP server not found" after the gate)
// through processToolCalls. Returns the final exec state and the set of
// logger event names emitted.
func runWriteAuthTurn(t *testing.T, requireWriteReason bool) (*toolExecState, map[string]bool) {
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
			return state, seen
		}
	}
}

func TestWriteAuthEvents_AdvisoryPathIsNotBlocked(t *testing.T) {
	_, seen := runWriteAuthTurn(t, false)
	if seen["EventWriteAuthBlocked"] {
		t.Error("advisory path (require_write_reason=false) must NOT emit EventWriteAuthBlocked — the call proceeds")
	}
	if !seen["EventWriteAuthNudged"] {
		t.Error("advisory path must emit EventWriteAuthNudged")
	}
}

func TestWriteAuthEvents_HardBlockPathIsBlocked(t *testing.T) {
	state, seen := runWriteAuthTurn(t, true)
	if !seen["EventWriteAuthBlocked"] {
		t.Error("hard-block path (require_write_reason=true) must emit EventWriteAuthBlocked")
	}
	if seen["EventWriteAuthNudged"] {
		t.Error("hard-block path must NOT emit EventWriteAuthNudged — the call is refused, not nudged")
	}
	// A3 side-effect on this path: the trace entry must hold the text the
	// model was actually shown (AUTHORIZATION REQUIRED), not the advisory
	// variant the early append used to record.
	if len(state.trace) != 1 {
		t.Errorf("one tool call must produce exactly 1 trace entry, got %d", len(state.trace))
	}
	if len(state.trace) == 1 && !strings.Contains(fmt.Sprintf("%v", state.trace[0].Result), "AUTHORIZATION REQUIRED") {
		t.Errorf("hard-block trace entry must record the AUTHORIZATION REQUIRED text the model saw, got: %v", state.trace[0].Result)
	}
}

// TestWriteAuthAdvisoryNudgeReachesModelContext is the A3 regression: the
// advisory nudge must be model-visible (toolResults), must carry the
// "do NOT repeat" guard (the call was already dispatched — a small model
// reading the warning as a complaint would otherwise re-issue the same
// write), and one call must produce exactly one trace entry (the early
// append used to double-book this path: one entry holding the nudge, one
// holding the final result).
func TestWriteAuthAdvisoryNudgeReachesModelContext(t *testing.T) {
	state, _ := runWriteAuthTurn(t, false)

	joined := strings.Join(state.toolResults, "\n")
	if !strings.Contains(joined, "changes external state") {
		t.Errorf("A3 REGRESSION: advisory nudge must reach the model (toolResults); got: %.200s", joined)
	}
	if !strings.Contains(joined, "do NOT repeat") {
		t.Error("A3 REGRESSION: advisory nudge must state the call is already executed — 'do NOT repeat' guards against duplicate writes")
	}
	// The real result must still follow the nudge — the call was dispatched
	// (here it fails because no MCP server is registered).
	if !strings.Contains(joined, "not found") {
		t.Error("A3 REGRESSION: the real tool result must still be appended after the nudge")
	}
	if len(state.trace) != 1 {
		t.Errorf("A3 REGRESSION: one tool call must produce exactly 1 trace entry, got %d (double-append regression)", len(state.trace))
	}
	if len(state.trace) == 1 && strings.Contains(fmt.Sprintf("%v", state.trace[0].Result), "changes external state") {
		t.Error("A3 REGRESSION: trace entry should hold the tool outcome, not the nudge text (nudge belongs to model context)")
	}
}
