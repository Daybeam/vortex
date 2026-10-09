package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	onChunk("Reading.")
	return &schemas.ProviderResponse{
		Text: "Reading.",
		ToolCalls: []schemas.ToolCall{
			{Name: "read_file", Arguments: map[string]any{"path": "x.txt"}},
		},
	}, nil
}

func (m *loopMockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *loopMockProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *loopMockProvider) Name() string                                              { return "loopmock" }

func TestChatHarnessLoopGuard(t *testing.T) {
	outBase := t.TempDir()
	taskDir := filepath.Join(outBase, "task1")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "x.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	h := &ChatHarness{
		Provider:   &loopMockProvider{},
		Model:      "mock",
		OutputBase: outBase,
		MaxTurns:   6,
		Sieve:      NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	var sawCacheReplay bool
	for _, ev := range events {
		if ev.Type == "cache_replay" {
			sawCacheReplay = true
		}
	}
	if !sawCacheReplay {
		t.Fatalf("expected cache_replay event for repeated tool call; got %v", events)
	}
}

type steerMockProvider struct {
	calls int
}

func (m *steerMockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *steerMockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= 3 {
		return &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": fmt.Sprintf("f%d.txt", m.calls), "content": "x"}},
			},
		}, nil
	}
	onChunk("Here is the answer.")
	return &schemas.ProviderResponse{Text: "Here is the answer."}, nil
}

func (m *steerMockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *steerMockProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *steerMockProvider) Name() string                                             { return "steermock" }

func TestChatHarnessSteerNoTextLoop(t *testing.T) {
	h := &ChatHarness{
		Provider:   &steerMockProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   6,
		Sieve:      NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	var sawSteer bool
	for _, ev := range events {
		if ev.Type == "steer" {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Fatalf("expected steer event for no-text loop; got %v", events)
	}
}

type writeCacheBugProvider struct {
	calls int
}

func (m *writeCacheBugProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *writeCacheBugProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= 2 {
		return &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": "x.txt", "content": "hello"}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *writeCacheBugProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *writeCacheBugProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *writeCacheBugProvider) Name() string                                             { return "writecachemock" }

func TestChatHarnessWriteToolNotCached(t *testing.T) {
	h := &ChatHarness{
		Provider:   &writeCacheBugProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   5,
		Sieve:      NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	for _, ev := range events {
		if ev.Type == "cache_replay" && ev.Data == "write_file" {
			t.Fatalf("write_file should never be cached; got cache_replay event: %v", events)
		}
	}
}
