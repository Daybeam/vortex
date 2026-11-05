package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// Q5-3 (decision 2026-11-05, option A): when the turn ceiling is reached the
// partial answer is kept, but kept *as* a partial. Two guarantees, both pinned
// here:
//
//	① the text is explicitly marked, so a transcript read later cannot mistake a
//	   half answer for a finished one;
//	② it never enters the archive retrieval index, so semantic recall cannot
//	   promote it to knowledge.
//
// Before the fix, Run returned the raw accumulated text with a plain
// fmt.Errorf("max turns (%d) exceeded") — no marker, no sentinel, and the API
// entry points discarded the whole wreck on any non-nil error.

// ceilingProvider produces prose AND a tool call on every turn: the shape that
// survives every guard in the loop (it is never text-less, never repeats a tool
// hash because the path changes) and therefore reaches the ceiling with a real
// partial in hand.
type ceilingProvider struct {
	calls int
}

func (p *ceilingProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *ceilingProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	text := fmt.Sprintf("step %d of the work. ", p.calls)
	onChunk(text)
	return &schemas.ProviderResponse{
		Text: text,
		ToolCalls: []schemas.ToolCall{
			{Name: "write_file", Arguments: map[string]any{
				"path":    fmt.Sprintf("part%d.txt", p.calls),
				"content": fmt.Sprintf("chunk %d", p.calls),
			}},
		},
	}, nil
}

func (p *ceilingProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (p *ceilingProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *ceilingProvider) Name() string                                              { return "ceilingmock" }

// ceilingEmbed is an EmbeddingClient stub with a non-nil vector, so indexTurn
// cannot silently return early on an embedding error and make the "not indexed"
// assertion pass for the wrong reason.
type ceilingEmbed struct{}

func (ceilingEmbed) Embed(ctx context.Context, text string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// newCeilingHarness builds a harness whose artifact dir exists, so write_file
// succeeds and the loop keeps going until the ceiling.
func newCeilingHarness(t *testing.T, prov providers.Provider, maxTurns int, archive *core.ContextArchive) (*ChatHarness, string) {
	t.Helper()
	outBase := t.TempDir()
	taskID := "task_ceiling"
	if err := os.MkdirAll(filepath.Join(outBase, taskID), 0755); err != nil {
		t.Fatal(err)
	}
	return &ChatHarness{
		Provider:   prov,
		Model:      "ceilingmock",
		OutputBase: outBase,
		MaxTurns:   maxTurns,
		Archive:    archive,
		Embed:      ceilingEmbed{},
	}, taskID
}

func archiveItems(t *testing.T, archive *core.ContextArchive) []core.MemoryItem {
	t.Helper()
	if err := archive.Load(); err != nil {
		t.Fatalf("archive.Load: %v", err)
	}
	return archive.Items()
}

// TestCeiling_PartialIsMarkedAndSentinelWrapped covers guarantee ① plus the
// machine-readable cause. Fails before the fix: text was unmarked and the error
// was a bare fmt.Errorf with no sentinel.
func TestCeiling_PartialIsMarkedAndSentinelWrapped(t *testing.T) {
	const maxTurns = 4
	h, taskID := newCeilingHarness(t, &ceilingProvider{}, maxTurns, nil)

	final, err := h.Run(context.Background(), taskID,
		[]ChatMessage{{Role: "user", Content: "do the long thing"}},
		func(ChatEvent) error { return nil })

	if err == nil {
		t.Fatal("expected the ceiling to return an error")
	}
	if !IsMaxTurnsPartial(err) {
		t.Fatalf("ceiling error is not ErrMaxTurnsExceeded: %v", err)
	}
	// The human-facing message shape is unchanged: callers that string-match it
	// (and chat_budget_test.go) must keep working.
	if want := fmt.Sprintf("max turns (%d) exceeded", maxTurns); !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if !IsTruncated(final) {
		t.Fatalf("partial is not marked; got %q", final)
	}
	if !strings.Contains(final, fmt.Sprintf("%d turns used", maxTurns)) {
		t.Errorf("marker does not state the turn count: %q", final)
	}
	// Keeping the partial means keeping its content, not replacing it.
	for i := 1; i <= maxTurns; i++ {
		if want := fmt.Sprintf("step %d of the work.", i); !strings.Contains(final, want) {
			t.Errorf("turn %d's prose missing from the marked partial: %q", i, final)
		}
	}
}

// TestCeiling_PartialStaysOutOfTheArchiveIndex covers guarantee ② on the harness
// side: indexTurn is gated on err == nil, so a ceiling run must contribute zero
// retrievable items — while a clean run through the same fixture contributes one.
func TestCeiling_PartialStaysOutOfTheArchiveIndex(t *testing.T) {
	archive := core.NewContextArchive(filepath.Join(t.TempDir(), "chat_ctx.jsonl"))

	h, taskID := newCeilingHarness(t, &ceilingProvider{}, 3, archive)
	_, err := h.Run(context.Background(), taskID,
		[]ChatMessage{{Role: "user", Content: "recall me on this"}},
		func(ChatEvent) error { return nil })
	if !IsMaxTurnsPartial(err) {
		t.Fatalf("expected a ceiling error, got %v", err)
	}
	if items := archiveItems(t, archive); len(items) != 0 {
		t.Fatalf("a truncated partial was indexed as knowledge: %+v", items)
	}

	// Positive control: the same wiring DOES index a complete turn, so the
	// assertion above cannot be passing because the fixture is inert.
	clean, _ := newCeilingHarness(t, &recallSimpleProvider{}, 1, archive)
	if _, err := clean.Run(context.Background(), taskID,
		[]ChatMessage{{Role: "user", Content: "recall me on this"}},
		func(ChatEvent) error { return nil }); err != nil {
		t.Fatalf("clean run failed: %v", err)
	}
	if items := archiveItems(t, archive); len(items) != 1 {
		t.Fatalf("expected the complete turn to index 1 item, got %d", len(items))
	}
}

// TestCeiling_EmptyWreckIsNotMarked: with no prose at all there is nothing to
// qualify, and a marker alone would read like an answer that said something.
func TestCeiling_EmptyWreckIsNotMarked(t *testing.T) {
	h, taskID := newCeilingHarness(t, &writeOnlyProvider{}, 3, nil)

	final, err := h.Run(context.Background(), taskID,
		[]ChatMessage{{Role: "user", Content: "keep writing"}},
		func(ChatEvent) error { return nil })

	if !IsMaxTurnsPartial(err) {
		t.Fatalf("expected a ceiling error, got %v", err)
	}
	if final != "" {
		t.Errorf("expected no marked text for an empty partial, got %q", final)
	}
	if IsTruncated(final) {
		t.Errorf("IsTruncated must be false for the empty string")
	}
}

// TestMarkTruncated_RoundTripsThroughIsTruncated pins the pair the API layer
// relies on: whatever markTruncated produces is what IsTruncated refuses.
func TestMarkTruncated_RoundTripsThroughIsTruncated(t *testing.T) {
	marked := markTruncated("half an answer", 7)
	if !IsTruncated(marked) {
		t.Fatalf("marked text not detected: %q", marked)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrMaxTurnsExceeded), ErrMaxTurnsExceeded) {
		t.Fatal("sentinel must survive wrapping")
	}
	if IsMaxTurnsPartial(errors.New("some other failure")) {
		t.Fatal("a real failure must not read as a ceiling")
	}
	if IsMaxTurnsPartial(nil) {
		t.Fatal("nil error is not a ceiling")
	}
}
