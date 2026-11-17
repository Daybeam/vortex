package chat

import (
	"bytes"
	"context"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Regression (V10 in docs/plan/VERIFIED_ISSUES_2026-11-11.md; spec §8.2 of
// docs/plan/DELEGATION_INTENT_TRIGGER_ANALYSIS.md): the summary line carried
// only aggregate counts, so neither "which tools did the model call" nor "how
// many were on the table" could be answered from a retained log. offered/called
// was uncomputable, and the trace that could answer it is rotated away (V5).
//
// The histogram counts requests, not successes: a call the harness rejects is
// still a call the model made, and dropping it would understate the waste the
// metric exists to expose.

func TestRunStats_HistogramIsSortedAndCountsRequests(t *testing.T) {
	s := newRunStats(0, 0)
	if got := s.histogram(); got != "na" {
		t.Errorf("a run that called nothing must still emit the field, got %q", got)
	}

	// Deliberately unsorted, with a repeat and two execute_code calls.
	for _, name := range []string{"read_file", "execute_code", "archive_search", "execute_code", "read_file", "read_file"} {
		s.recordTool(name)
	}

	if got, want := s.histogram(), "archive_search=1,execute_code=2,read_file=3"; got != want {
		t.Errorf("histogram must be sorted name=count: got %q, want %q", got, want)
	}
	if s.toolCalls != 6 {
		t.Errorf("toolCalls = %d, want 6", s.toolCalls)
	}
	// execute_code writes are invisible to writeTargetOf, so the separate
	// counter must keep tracking them after the move out of Run.
	if s.codeExec != 2 {
		t.Errorf("codeExec = %d, want 2", s.codeExec)
	}
}

// The hard post-delegation lockout removes the work tools mid-run. The
// denominator for offered/called is the surface the run started with, so a
// later, smaller table must not overwrite it.
func TestRunStats_OfferToolsKeepsTheFirstSurface(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "hard")
	h := &ChatHarness{}
	s := newRunStats(0, 0)

	full := len(s.offerTools(h))
	if full == 0 {
		t.Fatal("tool table is empty — the rest of this test cannot mean anything")
	}

	h.pd.delegatedHere = true
	shrunk := len(s.offerTools(h))
	if shrunk >= full {
		t.Fatalf("hard arm did not shrink the table (%d -> %d); the lockout is not armed", full, shrunk)
	}
	if s.offeredTools != full {
		t.Errorf("offeredTools = %d, want the first surface %d (not the post-lockout %d)", s.offeredTools, full, shrunk)
	}
}

// histogramProbeProvider asks for two unknown tools on turn 1 and one on turn
// 2, then answers with text. Unknown names are used on purpose: they fail
// inside execTool without touching the filesystem or running code, which is
// what makes "counts requests, not successes" observable.
type histogramProbeProvider struct{ calls int }

func (p *histogramProbeProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *histogramProbeProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	switch p.calls {
	case 1:
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{
			{Name: "probe_beta", Arguments: map[string]any{"attempt": 1}},
			{Name: "probe_alpha", Arguments: map[string]any{"attempt": 1}},
		}}, nil
	case 2:
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{
			{Name: "probe_alpha", Arguments: map[string]any{"attempt": 2}},
		}}, nil
	default:
		onChunk("Done.")
		return &schemas.ProviderResponse{Text: "Done."}, nil
	}
}

func (p *histogramProbeProvider) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (p *histogramProbeProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (p *histogramProbeProvider) Name() string { return "histogram-probe" }

func TestChatTurnSummary_CarriesToolHistogramAndOfferedTools(t *testing.T) {
	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prevOut)

	h := &ChatHarness{
		Provider:   &histogramProbeProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   5,
	}
	wantOffered := len(h.ToolDefinitions())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = h.Run(ctx, "task-histogram", []ChatMessage{{Role: "user", Content: "probe"}},
		func(ChatEvent) error { return nil })

	line := ""
	for _, candidate := range strings.Split(logs.String(), "\n") {
		if strings.Contains(candidate, "[chat-turn-summary] task=task-histogram") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatal("no [chat-turn-summary] line emitted for the run — the deferred metric did not fire")
	}

	for _, want := range []string{
		"tool_histogram=probe_alpha=2,probe_beta=1",
		"tool_calls=3",
		"turns=3",
		"offered_tools=" + strconv.Itoa(wantOffered),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("summary line missing %q.\nline: %s", want, line)
		}
	}
	// Existing keys must keep their names and order; the new state rides on the
	// end of the line (same contract as TestChatTurnSummary_CarriesContractObservation).
	if !strings.HasSuffix(line, "offered_tools="+strconv.Itoa(wantOffered)) {
		t.Errorf("new fields must be appended, not interleaved.\nline: %s", line)
	}
	if wantOffered == 0 {
		t.Error("offered_tools=0 would make offered/called meaningless")
	}
}
