package core

import (
	"context"
	"fmt"
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

// -----------------------------------------------------------------------------
// Feature: Mid-Stream Chunk Repetition Detection
// Invariant: Repetitive streaming output triggers steer+retry, not fatal error
// Scenario: Provider emits 65 identical chunks → detector fires → harness steers → retry succeeds
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 流式输出重复检测与恢复
// -----------------------------------------------------------------------------

type repetitiveStreamProvider struct {
	calls int
}

func (m *repetitiveStreamProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *repetitiveStreamProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls == 1 {
		// First call: emit 65 identical chunks — detector fires at 60
		for i := 0; i < 65; i++ {
			if err := onChunk("repeat"); err != nil {
				return nil, err // propagate ErrRepetitiveOutput
			}
		}
		return &schemas.ProviderResponse{Text: "repeat"}, nil
	}
	// Second call (after steer): normal varied output
	onChunk("Recovered answer.")
	return &schemas.ProviderResponse{Text: "Recovered answer."}, nil
}

func (m *repetitiveStreamProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *repetitiveStreamProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *repetitiveStreamProvider) Name() string { return "repetitive_mock" }

func TestChatHarnessStreamRepetitionRecovery(t *testing.T) {
	h := &ChatHarness{
		Provider:   &repetitiveStreamProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   5,
	}

	var events []ChatEvent
	final, err := h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Final text should be the recovered answer, not the repetitive output
	if final != "Recovered answer." {
		t.Fatalf("expected 'Recovered answer.', got %q", final)
	}

	// Verify loop_guard and steer events were emitted
	var sawLoopGuard, sawSteer bool
	for _, ev := range events {
		switch ev.Type {
		case "loop_guard":
			sawLoopGuard = true
		case "steer":
			sawSteer = true
		}
	}
	if !sawLoopGuard {
		t.Error("expected loop_guard event, not found")
	}
	if !sawSteer {
		t.Error("expected steer event, not found")
	}
}

// Fix ④: Non-read tools that succeed should get [ACTION COMPLETED] prefix
// so the model can distinguish write results from read results.
// See eval-kit/docs/28-engine-path-analysis.md §2 claim ④.
func TestChatHarness_WriteToolActionResultHasActionCompleted(t *testing.T) {
	h := &ChatHarness{
		Provider:   &writeCacheBugProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   5,
		Sieve:      NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task-ac",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	foundActionCompleted := false
	for _, ev := range events {
		if ev.Type == "tool_result" && ev.Meta["tool"] == "write_file" {
			if strings.HasPrefix(ev.Data, "[ACTION COMPLETED]") {
				foundActionCompleted = true
			}
		}
	}
	if !foundActionCompleted {
		t.Error("write_file tool_result should have [ACTION COMPLETED] prefix")
	}
}

// Fix ②: ReadinessGateSoft should soften the delegation readiness gate language.
// Default (false) keeps the strict "NEVER blindly delegate" wording.
// See eval-kit/docs/28-engine-path-analysis.md §2 claim ②.
func TestChatHarness_ReadinessGateSoft_ChangesPromptLanguage(t *testing.T) {
	h := &ChatHarness{Model: "test", OutputBase: t.TempDir()}

	strict := h.buildSystem("do something", "task1")
	if !strings.Contains(strict, "NEVER blindly delegate") {
		t.Error("strict mode should contain 'NEVER blindly delegate'")
	}
	if strings.Contains(strict, "consider asking the user for clarification") {
		t.Error("strict mode should NOT contain soft language")
	}

	h.ReadinessGateSoft = true
	soft := h.buildSystem("do something", "task1")
	if !strings.Contains(soft, "consider asking the user for clarification") {
		t.Error("soft mode should contain 'consider asking the user for clarification'")
	}
	if strings.Contains(soft, "NEVER blindly delegate") {
		t.Error("soft mode should NOT contain 'NEVER blindly delegate'")
	}
}
