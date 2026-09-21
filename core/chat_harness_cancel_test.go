package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression tests for M1: ctx.Err() check at the top of the turn loop in
// ChatHarness.Run (core/chat_harness.go).
//
// Before the fix, Run() did not check ctx.Err() inside the turn loop. If the
// context was cancelled mid-conversation (e.g. client disconnect or shutdown),
// the harness would keep calling the provider for the remaining turns instead
// of aborting. The fix added:
//   if ctx.Err() != nil {
//       emit(ChatEvent{Type: "error", Data: ...})
//       return finalText, ctx.Err()
//   }
// at the top of each turn iteration.

// neverCalledProvider is a provider that fails the test if any of its methods
// are called. Used to verify that a pre-cancelled context causes Run() to
// abort before the first provider call.
type neverCalledProvider struct {
	t *testing.T
}

func (p *neverCalledProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.t.Fatal("Complete should not be called when context is already cancelled")
	return nil, nil
}
func (p *neverCalledProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.t.Fatal("StreamComplete should not be called when context is already cancelled")
	return nil, nil
}
func (p *neverCalledProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	p.t.Fatal("Embed should not be called when context is already cancelled")
	return nil, nil
}
func (p *neverCalledProvider) CountTokens(ctx context.Context, text string) (int, error) {
	p.t.Fatal("CountTokens should not be called when context is already cancelled")
	return 0, nil
}
func (p *neverCalledProvider) Name() string { return "never-called" }

// TestChatHarness_PreCancelledContext_AbortsBeforeProviderCall verifies that
// when Run() is called with an already-cancelled context, it returns
// immediately without calling the provider. This is the core M1 invariant:
// the ctx.Err() check at the top of the turn loop prevents wasted work.
func TestChatHarness_PreCancelledContext_AbortsBeforeProviderCall(t *testing.T) {
	h := &ChatHarness{
		Provider:   &neverCalledProvider{t: t},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   4,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Run

	var events []ChatEvent
	final, err := h.Run(ctx, "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if err == nil {
		t.Fatal("expected non-nil error from Run() with cancelled context, got nil")
	}
	// Go's context.Canceled uses American spelling "canceled" (one 'l'),
	// while the chat_harness event data uses "cancelled" (two 'l's).
	// Match the prefix to cover both.
	if !strings.Contains(strings.ToLower(err.Error()), "context cancel") {
		t.Fatalf("expected 'context cancel' error, got: %v", err)
	}
	if final != "" {
		t.Fatalf("expected empty final text on cancelled context, got %q", final)
	}

	// Verify an error event was emitted.
	var sawErrorEvent bool
	for _, ev := range events {
		if ev.Type == "error" {
			sawErrorEvent = true
		}
	}
	if !sawErrorEvent {
		t.Fatal("expected an error event to be emitted on context cancellation")
	}
}

// countingProvider tracks how many times StreamComplete is called. On the
// first call it returns a write_file tool call (so the harness loop continues
// to the next turn); on subsequent calls it returns plain text. Used to
// verify that mid-loop cancellation stops further turns.
type countingProvider struct {
	calls int
}

func (p *countingProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}
func (p *countingProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	if p.calls == 1 {
		// First turn: return a tool call so the harness continues the
		// loop to a second iteration (where ctx.Err() should be checked).
		onChunk("Writing a file.")
		return &schemas.ProviderResponse{
			Text: "Writing a file.",
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": "cancel_test.txt", "content": "hi"}},
			},
		}, nil
	}
	onChunk("Turn response.")
	return &schemas.ProviderResponse{Text: "Turn response."}, nil
}
func (p *countingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *countingProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *countingProvider) Name() string                                              { return "counting" }

// TestChatHarness_CancelledAfterFirstTurn_AbortsSecondTurn verifies that if
// the context is cancelled between turns, the harness does not proceed to
// the next turn. We simulate this by cancelling the context in the emit
// callback after the first delta event.
func TestChatHarness_CancelledAfterFirstTurn_AbortsSecondTurn(t *testing.T) {
	provider := &countingProvider{}
	h := &ChatHarness{
		Provider:   provider,
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   10, // plenty of turns — the test verifies we don't use them all
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events []ChatEvent
	_, err := h.Run(ctx, "task1",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error {
			events = append(events, ev)
			// Cancel after the first delta — the next turn's ctx.Err()
			// check should catch this and abort.
			if ev.Type == "delta" {
				cancel()
			}
			return nil
		})

	// The harness should have returned an error (context cancelled) and
	// stopped after at most 1 provider call, not all 10 turns.
	if err == nil {
		t.Fatal("expected error from Run() after mid-loop cancellation, got nil")
	}
	if provider.calls > 1 {
		t.Fatalf("expected at most 1 provider call before cancellation was detected, got %d — ctx.Err() check not working", provider.calls)
	}
}
