package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Tests for turn budget exhaustion labeling (TURN_BUDGET_OUTCOME_LABELING.md §3).
//
// Before the fix, all errors from streamTurn — including context deadline
// exceeded from the turn budget or HTTP client timeout — were emitted as
// generic "error" events, causing downstream scoring to mislabel them as
// model_error. The fix checks ctx.Err() and errors.Is(err, context.DeadlineExceeded)
// before the generic emission and tags the event with meta.turn_budget_exhausted.

// fixedErrorProvider returns a predetermined error from StreamComplete.
type fixedErrorProvider struct{ err error }

func (p *fixedErrorProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}
func (p *fixedErrorProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return nil, p.err
}
func (p *fixedErrorProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *fixedErrorProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (p *fixedErrorProvider) Name() string { return "fixed-error" }

// ctxWaitingProvider blocks until ctx expires, then returns ctx.Err().
// Simulates a provider call that outlasts the turn budget.
type ctxWaitingProvider struct{}

func (p *ctxWaitingProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}
func (p *ctxWaitingProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *ctxWaitingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *ctxWaitingProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (p *ctxWaitingProvider) Name() string { return "ctx-waiting" }

// hasBudgetMeta checks whether any error event has turn_budget_exhausted=true.
func hasBudgetMeta(events []ChatEvent) bool {
	for _, ev := range events {
		if ev.Type == "error" && ev.Meta != nil {
			if v, ok := ev.Meta["turn_budget_exhausted"]; ok && v == true {
				return true
			}
		}
	}
	return false
}

// TestTurnBudget_ContextExpired_TagsAsBudgetExhausted verifies that when the
// turn context expires during a provider call, the error event is tagged with
// turn_budget_exhausted=true (not mislabeled as model_error).
//
// This is case (a) in the fix: ctx.Err() != nil.
func TestTurnBudget_ContextExpired_TagsAsBudgetExhausted(t *testing.T) {
	h := &ChatHarness{
		Provider:   &ctxWaitingProvider{},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var events []ChatEvent
	_, err := h.Run(ctx, "task_budget",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if err == nil {
		t.Fatal("expected error from Run() with expired context, got nil")
	}
	if !hasBudgetMeta(events) {
		t.Fatal("expected turn_budget_exhausted meta on error event")
	}
	// Verify the event data mentions "turn budget exhausted"
	for _, ev := range events {
		if ev.Type == "error" && strings.Contains(ev.Data, "turn budget exhausted") {
			return // found the right event
		}
	}
	t.Errorf("expected error event data to contain 'turn budget exhausted', got events: %v", events)
}

// TestTurnBudget_ProviderTimeout_TagsAsBudgetExhausted verifies that when the
// provider returns context.DeadlineExceeded but ctx is still alive (e.g.,
// http.Client.Timeout fired before the turn budget), the error event is still
// tagged with turn_budget_exhausted=true.
//
// This is case (b) in the fix: errors.Is(err, context.DeadlineExceeded) but
// ctx.Err() == nil. This happens when the provider's HTTP client timeout
// (120s for Anthropic/Gemini, 300s for OpenAI/Ollama) is shorter than the
// turn budget.
func TestTurnBudget_ProviderTimeout_TagsAsBudgetExhausted(t *testing.T) {
	h := &ChatHarness{
		Provider:   &fixedErrorProvider{err: context.DeadlineExceeded},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   1,
	}

	// ctx has a generous timeout — it will NOT expire.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var events []ChatEvent
	_, err := h.Run(ctx, "task_provider_timeout",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if err == nil {
		t.Fatal("expected error from Run() with provider timeout, got nil")
	}
	if !hasBudgetMeta(events) {
		t.Fatal("expected turn_budget_exhausted meta on error event for provider timeout")
	}
	// Verify the event data mentions "provider timeout"
	for _, ev := range events {
		if ev.Type == "error" && strings.Contains(ev.Data, "provider timeout") {
			return // found the right event
		}
	}
	t.Errorf("expected error event data to contain 'provider timeout', got events: %v", events)
}

// TestTurnBudget_ProviderError_NotTaggedAsBudgetExhausted verifies the reverse:
// when the provider returns a non-timeout error (e.g., 500 internal server
// error), the error event is NOT tagged with turn_budget_exhausted. This is a
// genuine model/provider failure and should be classified as model_error.
func TestTurnBudget_ProviderError_NotTaggedAsBudgetExhausted(t *testing.T) {
	providerErr := errors.New("provider returned 500: internal server error")
	h := &ChatHarness{
		Provider:   &fixedErrorProvider{err: providerErr},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var events []ChatEvent
	_, err := h.Run(ctx, "task_provider_error",
		[]ChatMessage{{Role: "user", Content: "test"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if err == nil {
		t.Fatal("expected error from Run() with provider error, got nil")
	}
	if hasBudgetMeta(events) {
		t.Fatal("did NOT expect turn_budget_exhausted meta for a provider 500 error")
	}
	// Verify the error event has the original error message (not "turn budget exhausted")
	for _, ev := range events {
		if ev.Type == "error" {
			if strings.Contains(ev.Data, "turn budget exhausted") || strings.Contains(ev.Data, "provider timeout") {
				t.Fatalf("generic provider error was mislabeled as budget/timeout: %s", ev.Data)
			}
			if !strings.Contains(ev.Data, "500") {
				t.Errorf("expected error event to contain '500', got: %s", ev.Data)
			}
			return
		}
	}
	t.Fatal("expected an error event to be emitted")
}
