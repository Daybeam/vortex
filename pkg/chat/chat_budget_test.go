package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/providers"
)

// TestChatHarness_ZeroMaxTurnsUsesDefault pins the loop's own fallback: a
// harness built without MaxTurns (any caller that bypasses
// pkg/api/chat_handlers.go) must get the 2026-11-04 budget, not the old 6.
//
// Regression the operator raised on 2026-11-04: 6 turns ended multi-step chat
// work mid-task, and the chat loop cannot be resumed the way the DAG path can.
func TestChatHarness_ZeroMaxTurnsUsesDefault(t *testing.T) {
	h := &ChatHarness{
		Provider:   &writeOnlyProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		// MaxTurns deliberately left at its zero value.
	}

	var events []ChatEvent
	_, err := h.Run(context.Background(), "task_budget_default",
		[]ChatMessage{{Role: "user", Content: "keep writing"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	want := fmt.Sprintf("max turns (%d) exceeded", DefaultMaxTurns)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected the loop to stop at %s, got err=%v", want, err)
	}
	for _, ev := range events {
		if ev.Type == "error" && strings.Contains(ev.Data, want) {
			return
		}
	}
	t.Errorf("no error event carried %q; last events: %v", want, events[len(events)-3:])
}

// ─── D3: a spent provider allowance is a ceiling, not a broken request ──────

// IsPartialRun decides whether the caller keeps the text a run produced. An
// exhausted provider budget stops the run mid-flight exactly like the turn
// ceiling does, so the text must survive — before D3 the run was reported as a
// plain failure and its output dropped.
func TestIsPartialRun_ProviderBudgetExhausted(t *testing.T) {
	err := &providers.BudgetExhaustedError{Msg: "maximum provider request budget of 40 was reached"}
	if !IsPartialRun(err) {
		t.Fatal("exhausted provider budget must count as a partial run")
	}
	if !IsPartialRun(fmt.Errorf("turn failed: %w", err)) {
		t.Fatal("exhausted provider budget must count as a partial run through wrapping")
	}
	if IsPartialRun(&providers.BadRequestError{Msg: "bad schema"}) {
		t.Fatal("a malformed request is a real failure, not a partial run")
	}
	if IsPartialRun(errors.New("connection reset by peer")) {
		t.Fatal("an unrecognized failure is not a partial run")
	}
}

// The transcript is what an operator reads at 3am; "bad request" for a spent
// budget cost three diagnoses on 2026-11-11. The label must be honest and the
// machine-readable flag must be there.
func TestEmitRunError_LabelsBudgetHonestly(t *testing.T) {
	var events []ChatEvent
	emit := func(ev ChatEvent) error { events = append(events, ev); return nil }

	emitRunError(emit, &providers.BudgetExhaustedError{
		Msg: `{"error":{"type":"max_agent_turns_exceeded"}}`,
	})

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != "error" {
		t.Fatalf("expected an error event, got %q", ev.Type)
	}
	if strings.Contains(ev.Data, "bad request:") {
		t.Fatalf("transcript still blames the request: %q", ev.Data)
	}
	if !strings.Contains(ev.Data, providers.BudgetExhaustedMarker) {
		t.Fatalf("transcript does not name the cause: %q", ev.Data)
	}
	if ev.Meta["provider_budget_exhausted"] != true {
		t.Fatalf("missing machine-readable flag, meta=%v", ev.Meta)
	}
}

// Everything else keeps the plain shape it had, flag-free.
func TestEmitRunError_OtherErrorsKeepTheirText(t *testing.T) {
	var events []ChatEvent
	emit := func(ev ChatEvent) error { events = append(events, ev); return nil }

	emitRunError(emit, &providers.BadRequestError{Msg: "invalid schema"})

	if len(events) != 1 || events[0].Data != "bad request: invalid schema" {
		t.Fatalf("expected the unchanged message, got %+v", events)
	}
	if _, ok := events[0].Meta["provider_budget_exhausted"]; ok {
		t.Fatal("non-budget errors must not carry the budget flag")
	}
}
