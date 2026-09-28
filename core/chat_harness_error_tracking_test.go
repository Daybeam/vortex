package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// mockFailingToolProvider returns tool calls to a non-existent tool with
// different args each time (to avoid the loop-guard signature check).
type mockFailingToolProvider struct {
	calls int
}

func (m *mockFailingToolProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *mockFailingToolProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= 4 {
		// Each call uses different args so the loop-guard signature differs.
		return &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "nonexistent_tool", Arguments: map[string]any{"attempt": m.calls}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *mockFailingToolProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *mockFailingToolProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *mockFailingToolProvider) Name() string { return "mock-failing" }

// TestChatHarness_ErrorTracking_InjectsErrorMessages is a regression test for
// the error-tracking upgrade in chat_harness.go: after 3 failures of the same
// tool, the model must receive the actual error messages (not just a generic
// "Stop using this tool") so it can learn the pattern and avoid it.
//
// Before the upgrade: after 3 failures, the result was
//   "[TOOL X HAS FAILED 3 TIMES] Stop using this tool..."
// After the upgrade: after 3 failures, the result is
//   "[TOOL X HAS FAILED 3 TIMES] Recent errors:\n<err1>\n<err2>\n<err3>..."
//
// Reproduction: call a non-existent tool 4 times; verify the 3rd+ result
// contains "Recent errors:" and the actual error text.
func TestChatHarness_ErrorTracking_InjectsErrorMessages(t *testing.T) {
	outBase := t.TempDir()
	h := &ChatHarness{
		Provider:   &mockFailingToolProvider{},
		Model:      "mock",
		OutputBase: outBase,
		MaxTurns:   10,
	}

	var events []ChatEvent
	_, _ = h.Run(context.Background(), "task-err-track",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	// Collect tool_result events for "nonexistent_tool".
	var toolResults []string
	for _, ev := range events {
		if ev.Type == "tool_result" {
			if tool, _ := ev.Meta["tool"].(string); tool == "nonexistent_tool" {
				toolResults = append(toolResults, ev.Data)
			}
		}
	}

	if len(toolResults) < 3 {
		t.Fatalf("expected at least 3 tool results, got %d", len(toolResults))
	}

	// First 2 results should use the per-error format (not "Recent errors:").
	for i := 0; i < 2; i++ {
		if strings.Contains(toolResults[i], "Recent errors:") {
			t.Errorf("result %d should not contain 'Recent errors:' yet (only %d failures)", i, i+1)
		}
	}

	// 3rd result must contain "Recent errors:" and the actual error text.
	if !strings.Contains(toolResults[2], "Recent errors:") {
		t.Errorf("3rd result should contain 'Recent errors:', got: %s", toolResults[2])
	}
	if !strings.Contains(toolResults[2], "unknown core tool: nonexistent_tool") {
		t.Errorf("3rd result should contain the actual error message, got: %s", toolResults[2])
	}

	// 3rd result must NOT contain the old generic "Stop using this tool" message.
	if strings.Contains(toolResults[2], "Stop using this tool") {
		t.Errorf("3rd result should not contain old generic 'Stop using this tool' message, got: %s", toolResults[2])
	}
}
