package chat

import (
	"github.com/daybeam/vortex/core/sieve"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Extracted from core/spawner_result_repetition_test.go during the pkg/chat
// extraction. Needs ChatHarness/ChatEvent/ChatMessage white-box access.

type resultRepetitionProvider struct {
	calls int
}

func (p *resultRepetitionProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(context.Background(), schemas.CompleteRequest{}, func(string) error { return nil })
}

func (p *resultRepetitionProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	// First 4 turns: call read_file on different files (all with same content).
	if p.calls <= 4 {
		return &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "read_file", Arguments: map[string]any{"path": filepath.Join("data", "file"+string(rune('0'+p.calls))+".txt")}},
			},
		}, nil
	}
	// Final turn: return a text answer.
	onChunk("Based on the data, the answer is 42.")
	return &schemas.ProviderResponse{Text: "Based on the data, the answer is 42."}, nil
}

func (p *resultRepetitionProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}
func (p *resultRepetitionProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (p *resultRepetitionProvider) Name() string { return "result-rep-mock" }

func TestChatHarness_ResultRepetition_E2E(t *testing.T) {
	taskID := "task-result-rep-e2e"
	outputBase := t.TempDir()
	dataDir := filepath.Join(outputBase, taskID, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	identicalContent := "The customer status is: active\n"
	for i := 1; i <= 5; i++ {
		filename := filepath.Join(dataDir, "file"+string(rune('0'+i))+".txt")
		if err := os.WriteFile(filename, []byte(identicalContent), 0644); err != nil {
			t.Fatal(err)
		}
	}

	sieve := sieve.NewSieve(100)
	sieve.ToolRepetitionThreshold = 3

	h := &ChatHarness{
		Provider:   &resultRepetitionProvider{},
		Model:      "mock",
		OutputBase: outputBase,
		MaxTurns:   10,
		Sieve:      sieve,
	}

	var events []ChatEvent
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.Run(ctx, taskID,
		[]ChatMessage{{Role: "user", Content: "Read the customer data files and summarize"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	sawResultRepetition := false
	for _, ev := range events {
		if ev.Type == "result_repetition" {
			sawResultRepetition = true
			t.Logf("result_repetition event emitted for tool: %s", ev.Data)
		}
	}
	if !sawResultRepetition {
		t.Error("Expected result_repetition event — CheckResultRepetition should have fired " +
			"after 3+ identical read_file results. This means the Sieve's result-level " +
			"repetition detection is not working.")
	}

	sawRepetitionMsg := false
	for _, ev := range events {
		if ev.Type == "tool_result" && strings.Contains(ev.Data, "[RESULT REPETITION]") {
			sawRepetitionMsg = true
		}
	}
	if !sawRepetitionMsg {
		t.Error("Expected [RESULT REPETITION] prefix in tool result — the steer message " +
			"should be prepended when CheckResultRepetition fires")
	}
}
