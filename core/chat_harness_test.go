package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

type mockChatProvider struct {
	calls int
}

func (m *mockChatProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *mockChatProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	switch m.calls {
	case 1:
		onChunk("Writing a file.")
		return &schemas.ProviderResponse{
			Text: "Writing a file.",
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": "x.txt", "content": "hello"}},
			},
		}, nil
	case 2:
		onChunk("Reading it back.")
		return &schemas.ProviderResponse{
			Text: "Reading it back.",
			ToolCalls: []schemas.ToolCall{
				{Name: "read_file", Arguments: map[string]any{"path": "x.txt"}},
			},
		}, nil
	default:
		onChunk("Done.")
		return &schemas.ProviderResponse{Text: "Done."}, nil
	}
}

func (m *mockChatProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *mockChatProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *mockChatProvider) Name() string { return "mock" }

func TestChatHarnessToolLoop(t *testing.T) {
	outBase := t.TempDir()
	h := &ChatHarness{
		Provider:   &mockChatProvider{},
		Model:      "mock",
		OutputBase: outBase,
		MaxTurns:   4,
	}

	var events []ChatEvent
	final, err := h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "store and read a file"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if final != "Writing a file.Reading it back.Done." {
		t.Fatalf("unexpected final text: %q", final)
	}

	var sawWrite, sawRead, sawDone bool
	for _, ev := range events {
		switch {
		case ev.Type == "tool_call" && ev.Data == "write_file":
			sawWrite = true
		case ev.Type == "tool_call" && ev.Data == "read_file":
			sawRead = true
		case ev.Type == "done":
			sawDone = true
		}
	}
	if !sawWrite || !sawRead || !sawDone {
		t.Fatalf("expected write_file, read_file, done events; got %v", events)
	}

	data, err := os.ReadFile(filepath.Join(outBase, "task1", "x.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("expected x.txt with 'hello', got %q err=%v", string(data), err)
	}
}

func TestChatHarnessDelegate(t *testing.T) {
	h := &ChatHarness{
		Provider:   &mockChatProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   4,
		Delegate:   func(prompt string) (string, error) { return "orch_123", nil },
	}
	if _, err := h.execTool(context.Background(), "delegate_to_orchestrator", map[string]any{"prompt": "do work"}, "task1"); err != nil {
		t.Fatalf("delegate failed: %v", err)
	}
	if _, err := h.execTool(context.Background(), "no_such_tool", map[string]any{}, "task1"); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

type loopMockProvider struct{}

func (m *loopMockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *loopMockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	if len(req.MCPServers) == 0 {
		onChunk("Final answer.")
		return &schemas.ProviderResponse{Text: "Final answer."}, nil
	}
	onChunk("Writing.")
	return &schemas.ProviderResponse{
		Text: "Writing.",
		ToolCalls: []schemas.ToolCall{
			{Name: "write_file", Arguments: map[string]any{"path": "x.txt", "content": "hi"}},
		},
	}, nil
}

func (m *loopMockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *loopMockProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *loopMockProvider) Name() string                                              { return "loopmock" }

func TestChatHarnessLoopGuard(t *testing.T) {
	h := &ChatHarness{
		Provider:   &loopMockProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   6,
	}
	var events []ChatEvent
	final, err := h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	var sawGuard bool
	for _, ev := range events {
		if ev.Type == "loop_guard" {
			sawGuard = true
		}
	}
	if !sawGuard {
		t.Fatalf("expected loop_guard event; got %v", events)
	}
	if !strings.Contains(final, "Final answer.") {
		t.Fatalf("expected forced final answer, got %q", final)
	}
}
