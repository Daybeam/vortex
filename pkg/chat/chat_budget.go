package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/providers"
)

// DefaultMaxTurns is the chat agent-loop budget when nothing configures it.
//
// 50 as of 2026-11-04. The previous 6 ended turns in the middle of multi-step
// work, and unlike the DAG path the chat loop has no way to pick a run back up:
// running out returns "max turns (N) exceeded" (chat_harness.go:1132-1133) and
// the task is over. A larger ceiling is the only mitigation until resume exists.
//
// Both entry points use it — the loop's own zero-value fallback
// (chat_harness.go:491-493) and pkg/api/chat_handlers.go, where config
// chat_max_turns / max_tool_turns can override it.
const DefaultMaxTurns = 50

// ErrMaxTurnsExceeded is the sentinel the loop returns when it reaches the
// ceiling without a final answer. Before Q5-3 the caller could only string-
// match "max turns (%d) exceeded" — which is how the two API entry points ended
// up treating the wreck like any other provider failure and discarding it, even
// though the text accumulated over N turns is worth more to the user than
// nothing. Callers that want to keep it must test this sentinel, not the string.
var ErrMaxTurnsExceeded = errors.New("chat turn budget exhausted")

// TruncatedMarkerPrefix is prepended to whatever partial text a ceiling-exhausted
// run produced. The marker is part of the returned string, not a side channel:
// it survives session persistence, so a transcript read days later still says
// "this answer was cut off" instead of reading like a complete one.
const TruncatedMarkerPrefix = "[PARTIAL — turn ceiling reached"

// markTruncated labels a ceiling-exhausted partial. An empty wreck stays empty —
// there is nothing to qualify, and a lone marker would look like an answer.
func markTruncated(text string, maxTurns int) string {
	if text == "" {
		return text
	}
	return fmt.Sprintf("%s: %d turns used] %s", TruncatedMarkerPrefix, maxTurns, text)
}

// IsMaxTurnsPartial reports whether a Run error is the turn ceiling rather than
// a real failure. Callers that keep only max-turns partials use this; new code
// should prefer IsPartialRun, which also keeps the exploration ceiling.
func IsMaxTurnsPartial(err error) bool {
	return errors.Is(err, ErrMaxTurnsExceeded)
}

// IsPartialRun reports whether a Run error is one of the deliberate ceilings —
// the turn budget, the opt-in exploration ceiling, or a provider-side request
// allowance the run cannot extend — rather than a real failure. Callers use it
// to keep the partial text; the ceilings share that contract but have different
// causes and different markers.
func IsPartialRun(err error) bool {
	if errors.Is(err, ErrMaxTurnsExceeded) || errors.Is(err, ErrExplorationExhausted) {
		return true
	}
	// D3 (2026-11-11): the gateway refused the next request because the run's
	// allowance is spent. Same family as the two ceilings above — the work
	// stopped mid-flight through no fault of the request, and whatever text it
	// produced is the only evidence it produced. Before this it fell through to
	// "turn failed" and the text was dropped.
	var budget *providers.BudgetExhaustedError
	return errors.As(err, &budget)
}

// emitTimeoutError checks whether a streamTurn error is a turn-budget or
// provider timeout and, if so, emits a labelled error event with the provider
// name and rate-limit details. Returns true if the caller should return (the
// error was a timeout); false if the caller should fall through to emitRunError.
//
// Two paths indicate a timeout:
//  (a) runCtx expired → ctx.Err() != nil. The request's context is ctx, so
//      client.Do returns context.DeadlineExceeded.
//  (b) http.Client.Timeout fired before runCtx → ctx.Err() == nil but the
//      error wraps context.DeadlineExceeded. This happens when the provider's
//      HTTP timeout (120s/300s) is shorter than the turn budget.
// Both are infrastructure timeouts, not model failures — the meta flag
// turn_budget_exhausted=true prevents downstream scoring from mislabelling
// them as model_error.
func emitTimeoutError(emit func(ChatEvent) error, err error, ctx context.Context, providerName string) bool {
	if ctxErr := ctx.Err(); ctxErr != nil {
		var rlErr *providers.RateLimitError
		if errors.As(err, &rlErr) {
			emit(ChatEvent{
				Type: "error",
				Data: fmt.Sprintf("turn timed out waiting for provider %s (rate limited, retry after %ds)", providerName, rlErr.RetryAfter),
				Meta: map[string]any{"turn_budget_exhausted": true, "provider": providerName, "rate_limited": true, "retry_after": rlErr.RetryAfter},
			})
		} else {
			emit(ChatEvent{
				Type: "error",
				Data: fmt.Sprintf("turn timed out waiting for provider %s: %v", providerName, ctxErr),
				Meta: map[string]any{"turn_budget_exhausted": true, "provider": providerName},
			})
		}
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		var rlErr *providers.RateLimitError
		if errors.As(err, &rlErr) {
			emit(ChatEvent{
				Type: "error",
				Data: fmt.Sprintf("provider %s timed out (rate limited, retry after %ds)", providerName, rlErr.RetryAfter),
				Meta: map[string]any{"turn_budget_exhausted": true, "provider": providerName, "rate_limited": true, "retry_after": rlErr.RetryAfter},
			})
		} else {
			emit(ChatEvent{
				Type: "error",
				Data: fmt.Sprintf("provider %s timed out (deadline exceeded): %v", providerName, err),
				Meta: map[string]any{"turn_budget_exhausted": true, "provider": providerName},
			})
		}
		return true
	}
	return false
}

// emitRunError presents a run-ending error to the transcript. It exists so the
// one error that must not read like a broken request — a provider refusing
// further calls because the allowance is gone — is labelled at the single place
// the transcript is written, instead of every reader re-deriving it from text.
//
// Before D3 this path emitted `bad request: {...}` for an exhausted budget,
// which is what made three runs (223 twice, 030 off r3) look like a broken
// mechanism rather than a spent budget. The meta flag is the machine-readable
// half: Data stays human text.
func emitRunError(emit func(ChatEvent) error, err error, providerName string) {
	var budget *providers.BudgetExhaustedError
	if errors.As(err, &budget) {
		emit(ChatEvent{
			Type: "error",
			Data: fmt.Sprintf("provider %s: %s", providerName, budget.Error()),
			Meta: map[string]any{"provider_budget_exhausted": true, "provider": providerName},
		})
		return
	}
	var rlErr *providers.RateLimitError
	if errors.As(err, &rlErr) {
		meta := map[string]any{"provider": providerName, "rate_limited": true}
		if rlErr.RetryAfter > 0 {
			meta["retry_after"] = rlErr.RetryAfter
		}
		emit(ChatEvent{
			Type: "error",
			Data: fmt.Sprintf("provider %s: %s", providerName, rlErr.Error()),
			Meta: meta,
		})
		return
	}
	emit(ChatEvent{Type: "error", Data: fmt.Sprintf("provider %s: %s", providerName, err.Error())})
}

// ErrExplorationExhausted is the sentinel the opt-in escalate arm returns when
// a run keeps exploring without writing a file past the last threshold. It is
// not a failure: the partial text is the only evidence the run produced and
// must be kept exactly like a max-turns wreck.
var ErrExplorationExhausted = errors.New("chat exploration budget exhausted without a write")

// ExplorationTruncatedPrefix labels an exploration-ceiling partial. The reason
// is part of the text because the follow-up turn reads only this string.
const ExplorationTruncatedPrefix = "[PARTIAL — exploration ceiling reached"

// markExplorationTruncated labels an exploration-ceiling partial. An empty
// wreck stays empty for the same reason as markTruncated.
func markExplorationTruncated(text string, calls int) string {
	if text == "" {
		return text
	}
	return fmt.Sprintf("%s: %d calls without a write] %s", ExplorationTruncatedPrefix, calls, text)
}

// IsTruncated reports whether a final text is a marked partial. Retrieval
// indexes must refuse these (Q5-3: a half answer embedded as knowledge
// outranks the complete answer that would have followed); callers should gate
// on err == nil first and use this as the second, content-level veto.
func IsTruncated(text string) bool {
	return strings.HasPrefix(text, TruncatedMarkerPrefix)
}
