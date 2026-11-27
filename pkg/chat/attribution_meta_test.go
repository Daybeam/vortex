package chat

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

type toolCallProvider struct{}

func (p *toolCallProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *toolCallProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	onChunk("running")
	return &schemas.ProviderResponse{
		Text: "done",
		ToolCalls: []schemas.ToolCall{
			{Name: "read_file", Arguments: map[string]any{"path": "test.txt"}},
		},
	}, nil
}

func (p *toolCallProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (p *toolCallProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (p *toolCallProvider) Name() string { return "tool-call-mock" }

// runAndCollectToolCalls runs one turn against the stub provider and returns
// the tool_call events it emitted.
func runAndCollectToolCalls(t *testing.T, h *ChatHarness, taskID string) []ChatEvent {
	t.Helper()

	var events []ChatEvent
	emit := func(ev ChatEvent) error { events = append(events, ev); return nil }

	_, _ = h.Run(context.Background(), taskID,
		[]ChatMessage{{Role: "user", Content: "read test.txt"}},
		emit)

	var toolCalls []ChatEvent
	for _, ev := range events {
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev)
		}
	}
	if len(toolCalls) == 0 {
		t.Fatal("expected at least one tool_call event")
	}
	return toolCalls
}

// The chat scope id goes out as `session_task_id`, never as `task_id`.
//
// On the same event stream `meta.task_id` already means the orchestrator task
// (`task_<hex>`): 3518 occurrences over the archived corpus, on progress /
// task_callback / followup_turn, 100% of them `task_*`. The value here is the
// chat scope id (`chat_<sessionID>`, chat_handlers.go:671) — a different
// namespace. Emitting it under `task_id` would make one key carry two kinds of
// identifier in one stream, so any consumer that joins on it silently mixes
// them.
func TestToolCallEvent_CarriesSessionTaskIDNotTaskID(t *testing.T) {
	h := &ChatHarness{
		Provider: &toolCallProvider{},
		Model:    "mock",
		Role:     &config.Role{ID: "analyst"},
		MaxTurns: 1,
	}

	for _, ev := range runAndCollectToolCalls(t, h, "chat_abc123") {
		got, ok := ev.Meta["session_task_id"]
		if !ok {
			t.Errorf("tool_call event missing session_task_id in Meta: %+v", ev.Meta)
			continue
		}
		if got != "chat_abc123" {
			t.Errorf("session_task_id = %v, want %q", got, "chat_abc123")
		}
		if _, clash := ev.Meta["task_id"]; clash {
			t.Errorf("tool_call event must not carry task_id (reserved for task_* on this stream): %+v", ev.Meta)
		}
	}
}

// A harness with no role must not report one.
//
// The turn emit wrapper in chat_handlers.go:878-887 stamps `role_id`/`role_name`
// whenever harness.Role is non-nil; that is the only place a role is attached.
// Over 742 archived round-files it fires on 1252 of 8934 tool_call events (14%),
// and the only value it ever writes is `chat_front_desk` — 602 of those files
// carry no role at all. If the harness also invented `orchestrator_default` for
// a nil Role, "unroled" and "a role with that id" would become the same string,
// and the attribution question "which role did this turn run as" would have no
// honest answer.
func TestToolCallEvent_UnboundRoleStampsNothing(t *testing.T) {
	h := &ChatHarness{
		Provider: &toolCallProvider{},
		Model:    "mock",
		MaxTurns: 1,
	}

	for _, ev := range runAndCollectToolCalls(t, h, "") {
		for _, key := range []string{"role", "role_id", "role_name"} {
			if _, present := ev.Meta[key]; present {
				t.Errorf("unroled tool_call event must not carry %q: %+v", key, ev.Meta)
			}
		}
		got, ok := ev.Meta["session_task_id"]
		if !ok {
			t.Fatalf("tool_call event missing session_task_id in Meta: %+v", ev.Meta)
		}
		if got != "" {
			t.Errorf("session_task_id = %v, want empty string for a non-task turn", got)
		}
	}
}

// The max_turns error carries the reason and the cap in Meta, while Data stays
// byte-identical: an existing consumer (or a log grepper) keying on
// "max turns (N) exceeded" keeps working.
func TestMaxTurnsError_HasExitReasonMeta(t *testing.T) {
	h := &ChatHarness{
		Provider: &toolCallProvider{},
		Model:    "mock",
		MaxTurns: 1,
	}

	var events []ChatEvent
	emit := func(ev ChatEvent) error { events = append(events, ev); return nil }

	_, _ = h.Run(context.Background(), "task-g4-test",
		[]ChatMessage{{Role: "user", Content: "read test.txt"}},
		emit)

	var maxTurnsEvent *ChatEvent
	for i := range events {
		if events[i].Type != "error" || events[i].Meta == nil {
			continue
		}
		if er, ok := events[i].Meta["exit_reason"].(string); ok && er == "max_turns" {
			maxTurnsEvent = &events[i]
			break
		}
	}
	if maxTurnsEvent == nil {
		t.Fatalf("no error event with meta.exit_reason == \"max_turns\" found in %d events", len(events))
	}

	if maxTurnsEvent.Meta["cap"] != 1 {
		t.Errorf("meta.cap = %v, want 1", maxTurnsEvent.Meta["cap"])
	}

	if maxTurnsEvent.Data != "max turns (1) exceeded" {
		t.Errorf("Data = %q, want %q (must be unchanged for backward compat)", maxTurnsEvent.Data, "max turns (1) exceeded")
	}
}
