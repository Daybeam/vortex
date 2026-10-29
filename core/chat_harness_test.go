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
	// MOCK-BOUNDARY (B1): Uses mockChatProvider (canned responses) — validates
	// harness turn loop / tool dispatch / steer logic, but NOT real LLM API
	// integration. Verification checkpoint: run with real SCNet/OpenAI endpoint
	// and assert the LLM produces correct tool calls for a novel task.
	// See docs/MOCK_BOUNDARY_TEST_SCENARIOS.md §B1.
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
func (m *steerMockProvider) Name() string                                              { return "steermock" }

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
func (m *writeCacheBugProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *writeCacheBugProvider) Name() string { return "writecachemock" }

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
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: streaming output repetition detection and recovery
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

// -----------------------------------------------------------------------------
// Tests for Tiered Auto-Handling of Oversized Chat Inputs
// See chat_harness.go §"Tiered Auto-Handling for Oversized Inputs"
// -----------------------------------------------------------------------------

// TestEnforceTokenBudget_TruncatesInsteadOfDropping verifies the fix for the
// silent-drop bug: when the newest message alone exceeds the token budget,
// it is truncated with a marker instead of being silently dropped.
func TestEnforceTokenBudget_TruncatesInsteadOfDropping(t *testing.T) {
	m := &RollingWindowMemory{WindowSize: 10, MaxTokens: 50} // very small budget
	// Create a message that alone exceeds 50 tokens (200 chars).
	bigContent := strings.Repeat("x", 500)
	history := []ChatMessage{
		{Role: "system", Content: "Summary"},
		{Role: "user", Content: bigContent},
	}
	result := m.enforceTokenBudget(history)
	if len(result) == 0 {
		t.Fatal("expected non-empty result, got empty (message was silently dropped)")
	}
	// The last message should be the truncated user message, not dropped.
	last := result[len(result)-1]
	if last.Role != "user" {
		t.Fatalf("expected last message to be user, got %s", last.Role)
	}
	if !strings.Contains(last.Content, "truncated") {
		t.Errorf("expected truncation marker in content, got: %s", last.Content[:min(100, len(last.Content))])
	}
	if len(last.Content) >= len(bigContent) {
		t.Error("expected content to be truncated (shorter than original)")
	}
}

// TestReadFile_StartLineEndLine verifies that read_file with start_line/end_line
// returns only the specified line range.
func TestReadFile_StartLineEndLine(t *testing.T) {
	outBase := t.TempDir()
	taskDir := filepath.Join(outBase, "task1")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write a 10-line file.
	content := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10"
	if err := os.WriteFile(filepath.Join(taskDir, "test.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Read lines 3-5.
	result, err := HandleCoreTool(context.Background(), "read_file", map[string]any{
		"path":       "test.txt",
		"start_line": float64(3),
		"end_line":   float64(5),
	}, outBase, "task1")
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if !strings.Contains(result, "Lines 3-5 of 10") {
		t.Errorf("expected line range header, got: %s", result)
	}
	if !strings.Contains(result, "line3") || !strings.Contains(result, "line5") {
		t.Errorf("expected line3 and line5 in result, got: %s", result)
	}
	if strings.Contains(result, "line2") || strings.Contains(result, "line6") {
		t.Errorf("should not contain line2 or line6, got: %s", result)
	}
}

// TestReadFile_StartLineEndLine_BeyondEnd verifies graceful handling when
// start_line is beyond the file end.
func TestReadFile_StartLineEndLine_BeyondEnd(t *testing.T) {
	outBase := t.TempDir()
	taskDir := filepath.Join(outBase, "task1")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "line1\nline2\nline3"
	if err := os.WriteFile(filepath.Join(taskDir, "test.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := HandleCoreTool(context.Background(), "read_file", map[string]any{
		"path":       "test.txt",
		"start_line": float64(100),
	}, outBase, "task1")
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if !strings.Contains(result, "beyond file end") {
		t.Errorf("expected 'beyond file end' message, got: %s", result)
	}
}

// tier1Provider is a mock provider that just returns a text response.
type tier1Provider struct{}

func (m *tier1Provider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *tier1Provider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	onChunk("Response received.")
	return &schemas.ProviderResponse{Text: "Response received."}, nil
}

func (m *tier1Provider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (m *tier1Provider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *tier1Provider) Name() string                                              { return "tier1mock" }

// TestChatHarness_Tier1_LargeInputMountedAsFile verifies that when the user's
// input exceeds 50% of MaxContextWindow, it is mounted as a file and the
// message is replaced with a summary + file reference.
func TestChatHarness_Tier1_LargeInputMountedAsFile(t *testing.T) {
	outBase := t.TempDir()
	h := &ChatHarness{
		Provider:         &tier1Provider{},
		Model:            "mock",
		OutputBase:       outBase,
		MaxTurns:         2,
		MaxContextWindow: 2000, // 50% = 1000 tokens = 4000 chars
	}

	// Create a large user message: 5000 chars → 1250 tokens > 1000 (50% of 2000).
	// And len > 4000 to satisfy the threshold check.
	largeContent := strings.Repeat("This is a line of content.\n", 200) // ~5800 chars
	var events []ChatEvent
	_, err := h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: largeContent}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Verify a steer event was emitted indicating the input was mounted.
	var sawSteer bool
	for _, ev := range events {
		if ev.Type == "steer" && strings.Contains(ev.Data, "mounted as file") {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Errorf("expected steer event for file mount, got events: %v", events)
	}

	// Verify the file was actually written.
	entries, err := os.ReadDir(filepath.Join(outBase, "task1", "inputs"))
	if err != nil {
		t.Fatalf("expected inputs dir, got error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one mounted file in inputs dir")
	}
}

// tier3Provider is a mock provider that should NOT be called (Tier 3 delegates).
type tier3Provider struct{ called bool }

func (m *tier3Provider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *tier3Provider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.called = true
	onChunk("Should not reach here.")
	return &schemas.ProviderResponse{Text: "Should not reach here."}, nil
}

func (m *tier3Provider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (m *tier3Provider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *tier3Provider) Name() string                                              { return "tier3mock" }

// TestChatHarness_Tier3_AutoDelegate verifies that when the user's input
// exceeds 100% of MaxContextWindow, it is auto-delegated to the orchestrator.
func TestChatHarness_Tier3_AutoDelegate(t *testing.T) {
	outBase := t.TempDir()
	provider := &tier3Provider{}
	delegateCalled := false
	h := &ChatHarness{
		Provider:         provider,
		Model:            "mock",
		OutputBase:       outBase,
		MaxTurns:         2,
		MaxContextWindow: 1000, // 100% = 1000 tokens = 4000 chars
		Delegate: func(prompt string) (string, error) {
			delegateCalled = true
			return "orch_task_456", nil
		},
	}

	// Create a very large user message: 5000 chars → 1250 tokens > 1000 (MaxContextWindow).
	largeContent := strings.Repeat("This is a line of content.\n", 200)
	var events []ChatEvent
	_, err := h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: largeContent}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if !delegateCalled {
		t.Error("expected Delegate to be called for Tier 3 auto-delegation")
	}
	if provider.called {
		t.Error("provider should NOT have been called — input should have been delegated")
	}

	// Verify auto_delegated_to_orchestrator event was emitted.
	var sawDelegate bool
	for _, ev := range events {
		if ev.Type == "tool_call" && ev.Data == "auto_delegated_to_orchestrator" {
			sawDelegate = true
		}
	}
	if !sawDelegate {
		t.Errorf("expected auto_delegated_to_orchestrator event, got: %v", events)
	}
}

// overflowProvider returns a context limit error on first call, then normal response.
type overflowProvider struct {
	calls int
}

func (m *overflowProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *overflowProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls == 1 {
		return nil, fmt.Errorf("context_length_exceeded: maximum context length is 4096 tokens")
	}
	onChunk("Recovered after truncation.")
	return &schemas.ProviderResponse{Text: "Recovered after truncation."}, nil
}

func (m *overflowProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *overflowProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *overflowProvider) Name() string                                              { return "overflowmock" }

// TestChatHarness_Tier2_ContextOverflowRecovery verifies that when the provider
// returns a context_length_exceeded error, the harness truncates the input
// and retries.
func TestChatHarness_Tier2_ContextOverflowRecovery(t *testing.T) {
	h := &ChatHarness{
		Provider:   &overflowProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   3,
	}

	// Create a large user message that would trigger overflow.
	largeContent := strings.Repeat("This is a line of content.\n", 200)
	var events []ChatEvent
	final, err := h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: largeContent}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Verify the response is the recovered text, not an error.
	if !strings.Contains(final, "Recovered") {
		t.Errorf("expected recovered response, got: %q", final)
	}

	// Verify steer event for overflow was emitted.
	var sawOverflowSteer bool
	for _, ev := range events {
		if ev.Type == "steer" && strings.Contains(ev.Data, "context overflow") {
			sawOverflowSteer = true
		}
	}
	if !sawOverflowSteer {
		t.Errorf("expected steer event for context overflow, got: %v", events)
	}
}

// -----------------------------------------------------------------------------
// Tests for thinkingExtractor (imd/} tag separation)
// -----------------------------------------------------------------------------

// hex escapes for < and > to avoid editor stripping angle brackets.
const (
	testOpenTag  = "\x3cthink\x3e"  // imd
	testCloseTag = "\x3c/think\x3e" // }
)

func TestThinkingExtractor_SimpleTags(t *testing.T) {
	te := &thinkingExtractor{}
	input := testOpenTag + "reasoning here" + testCloseTag + "actual content"
	content, thinking := te.Process(input)
	c2, t2 := te.Flush()
	content += c2
	thinking += t2
	if thinking != "reasoning here" {
		t.Errorf("expected thinking 'reasoning here', got %q", thinking)
	}
	if content != "actual content" {
		t.Errorf("expected content 'actual content', got %q", content)
	}
}

func TestThinkingExtractor_NoTags(t *testing.T) {
	te := &thinkingExtractor{}
	content, thinking := te.Process("just regular content")
	c2, t2 := te.Flush()
	content += c2
	thinking += t2
	if thinking != "" {
		t.Errorf("expected no thinking, got %q", thinking)
	}
	if content != "just regular content" {
		t.Errorf("expected 'just regular content', got %q", content)
	}
}

func TestThinkingExtractor_SplitTags(t *testing.T) {
	te := &thinkingExtractor{}
	// Tag split across chunks: open tag in chunk 1, content + close tag in chunk 2
	content, thinking := te.Process(testOpenTag)
	if content != "" || thinking != "" {
		t.Errorf("expected empty results for partial tag, got content=%q thinking=%q", content, thinking)
	}
	content, thinking = te.Process("reasoning" + testCloseTag)
	c2, t2 := te.Flush()
	content += c2
	thinking += t2
	if thinking != "reasoning" {
		t.Errorf("expected thinking 'reasoning', got %q", thinking)
	}
	if content != "" {
		t.Errorf("expected no content, got %q", content)
	}
}

func TestThinkingExtractor_MultipleBlocks(t *testing.T) {
	te := &thinkingExtractor{}
	input := testOpenTag + "think1" + testCloseTag + "text1" + testOpenTag + "think2" + testCloseTag + "text2"
	content, thinking := te.Process(input)
	c2, t2 := te.Flush()
	content += c2
	thinking += t2
	if thinking != "think1think2" {
		t.Errorf("expected 'think1think2', got %q", thinking)
	}
	if content != "text1text2" {
		t.Errorf("expected 'text1text2', got %q", content)
	}
}

func TestThinkingExtractor_Flush(t *testing.T) {
	te := &thinkingExtractor{}
	_, thinking := te.Process(testOpenTag + "partial reasoning")
	// No closing tag — flush should return remaining as thinking
	_, t2 := te.Flush()
	thinking += t2
	if thinking != "partial reasoning" {
		t.Errorf("expected 'partial reasoning', got %q", thinking)
	}
}

// ── Gap-3: Fallback Windowed Loop Detection (Sieve=nil) ─────────────────────

type fallbackLoopProvider struct {
	calls int
}

func (m *fallbackLoopProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *fallbackLoopProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= 5 {
		return &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "read_file", Arguments: map[string]any{"path": "x.txt"}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *fallbackLoopProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *fallbackLoopProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *fallbackLoopProvider) Name() string { return "fallbackmock" }

func TestChatHarness_FallbackWindowedLoop_WhenSieveNil(t *testing.T) {
	outBase := t.TempDir()
	taskDir := filepath.Join(outBase, "task1")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "x.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Sieve is nil — exercises the fallback windowed loop detection (Gap-3).
	h := &ChatHarness{
		Provider:   &fallbackLoopProvider{},
		Model:      "mock",
		OutputBase: outBase,
		MaxTurns:   8,
		// Sieve intentionally nil
	}

	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	var sawFallbackGuard bool
	for _, ev := range events {
		if ev.Type == "loop_guard" && strings.Contains(ev.Data, "windowed fallback") {
			sawFallbackGuard = true
		}
	}
	if !sawFallbackGuard {
		t.Error("expected loop_guard event with 'windowed fallback' when Sieve is nil")
	}
}

// genericErrorProvider always fails StreamComplete with a non-context,
// non-repetitive error — exercising the generic provider-error branch.
type genericErrorProvider struct{ calls int }

func (p *genericErrorProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}
func (p *genericErrorProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	return nil, fmt.Errorf("upstream provider unreachable")
}
func (p *genericErrorProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *genericErrorProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (p *genericErrorProvider) Name() string { return "generic-error" }

// TestChatHarness_ProviderError_Generic_SurfacesAndReturns covers the generic
// provider-error branch in chat_harness.go — the fall-through reached when the
// error is neither ErrRepetitiveOutput (steer+retry) nor a context-limit
// overflow (truncate+retry). A plain provider failure must emit an "error"
// event AND return that error to the caller, rather than being swallowed by a
// recovery path or looping until MaxTurns.
//
// Non-redundant: the two cancellation tests exercise the ctx.Err() path, and
// TestChatHarness_Tier2_ContextOverflowRecovery exercises the overflow path;
// none assert the plain provider-error branch.
func TestChatHarness_ProviderError_Generic_SurfacesAndReturns(t *testing.T) {
	provider := &genericErrorProvider{}
	h := &ChatHarness{
		Provider:   provider,
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   4,
	}

	var events []ChatEvent
	_, err := h.Run(context.Background(), "task-provider-err",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if err == nil {
		t.Fatal("expected Run() to return the provider error, got nil")
	}
	if !strings.Contains(err.Error(), "upstream provider unreachable") {
		t.Fatalf("expected provider error to propagate, got: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly 1 provider call before surfacing the error, got %d", provider.calls)
	}

	var sawErrorEvent bool
	for _, ev := range events {
		if ev.Type == "error" && strings.Contains(ev.Data, "upstream provider unreachable") {
			sawErrorEvent = true
		}
	}
	if !sawErrorEvent {
		t.Fatal("expected an 'error' event carrying the provider error message")
	}
}
