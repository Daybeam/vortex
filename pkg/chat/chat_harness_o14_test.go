package chat

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// Regression coverage for O14: Run was split into private per-run phases. These
// tests pin the ordering that crossed the extraction seams: provider/tool/result
// SSE events, the exact offered allowlist, currentTurn cleanup, and Tier-3's
// early return before summary/index defers are armed.
type o14Provider struct {
	calls    int
	requests []schemas.CompleteRequest
}

func (p *o14Provider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *o14Provider) StreamComplete(_ context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	p.requests = append(p.requests, req)
	if p.calls == 1 {
		onChunk("Writing then reading. ")
		return &schemas.ProviderResponse{
			Text: "Writing then reading. ",
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": "o14.txt", "content": "ok"}},
				{Name: "read_file", Arguments: map[string]any{"path": "o14.txt"}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (p *o14Provider) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (p *o14Provider) CountTokens(context.Context, string) (int, error) { return 0, nil }
func (p *o14Provider) Name() string                                     { return "o14" }

func TestChatHarnessO14ExtractedLoopPreservesEventAndToolOrder(t *testing.T) {
	provider := &o14Provider{}
	h := &ChatHarness{
		Provider:   provider,
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   3,
	}
	wantTools := h.ToolDefinitions()
	var events []ChatEvent

	final, err := h.Run(context.Background(), "o14-order",
		[]ChatMessage{{Role: "user", Content: "write and read"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "Writing then reading. Done."; final != want {
		t.Fatalf("final = %q, want %q", final, want)
	}
	if h.currentTurn != 0 {
		t.Fatalf("currentTurn = %d after Run, want 0", h.currentTurn)
	}

	var got []string
	for _, ev := range events {
		switch ev.Type {
		case "delta":
			// streamTurn may split one provider chunk into several SSE deltas;
			// compare the phase boundary, not its internal chunk size.
			if n := len(got); n > 0 && strings.HasPrefix(got[n-1], "delta:") {
				got[n-1] += ev.Data
			} else {
				got = append(got, ev.Type+":"+ev.Data)
			}
		case "done":
			got = append(got, ev.Type+":"+ev.Data)
		case "tool_call":
			got = append(got, ev.Type+":"+ev.Data)
		case "tool_result":
			tool, _ := ev.Meta["tool"].(string)
			got = append(got, ev.Type+":"+tool)
		}
	}
	want := []string{
		"delta:Writing then reading. ",
		"tool_call:write_file",
		"tool_result:write_file",
		"tool_call:read_file",
		"tool_result:read_file",
		"delta:Done.",
		"done:",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %#v, want %#v", got, want)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.requests))
	}
	for turn, req := range provider.requests {
		if len(req.MCPServers) != 1 || req.MCPServers[0].Name != "core" {
			t.Fatalf("turn %d MCP servers = %#v, want one core server", turn, req.MCPServers)
		}
		if gotTools := req.MCPServers[0].Tools; !reflect.DeepEqual(gotTools, wantTools) {
			t.Fatalf("turn %d tool allowlist changed during extraction", turn)
		}
	}
}

func TestChatHarnessO14TierThreeReturnSkipsLoopDefers(t *testing.T) {
	provider := &neverCalledProvider{t: t}
	archive := core.NewContextArchive(filepath.Join(t.TempDir(), "chat_ctx.jsonl"))
	h := &ChatHarness{
		Provider:         provider,
		Model:            "mock",
		OutputBase:       t.TempDir(),
		MaxContextWindow: 1000,
		MaxTurns:         2,
		Archive:          archive,
		Embed:            &recallMockEmbed{vec: []float32{0.1, 0.2, 0.3}},
		Delegate:         func(string, int) (string, error) { return "orch_o14", nil },
	}
	var events []ChatEvent
	final, err := h.Run(context.Background(), "o14-tier3",
		[]ChatMessage{{Role: "user", Content: strings.Repeat("large input\n", 400)}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(final, "delegated analysis to the orchestrator") {
		t.Fatalf("unexpected Tier-3 output: %q", final)
	}
	if h.currentTurn != 0 {
		t.Fatalf("currentTurn = %d after Tier-3 return, want 0", h.currentTurn)
	}
	if items := archiveItems(t, archive); len(items) != 0 {
		t.Fatalf("Tier-3 early return indexed %d turns; want none", len(items))
	}
	var got []string
	for _, ev := range events {
		got = append(got, ev.Type+":"+ev.Data)
	}
	want := []string{
		"tool_call:auto_delegated_to_orchestrator",
		"tool_result:" + final,
		"done:",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tier-3 events = %#v, want %#v", got, want)
	}
}
