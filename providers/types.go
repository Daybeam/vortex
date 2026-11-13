package providers

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/pkg/ratelimit"
	"github.com/daybeam/vortex/schemas"
)

// ─── Errors ───────────────────────────────────────────────────────────────

type RateLimitError struct {
	Msg        string
	RetryAfter int
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited (retry after %ds): %s", e.RetryAfter, e.Msg)
	}
	return "rate limited: " + e.Msg
}

type BadRequestError struct{ Msg string }

func (e *BadRequestError) Error() string { return "bad request: " + e.Msg }

// BudgetExhaustedError is a 400 whose body says the caller's *allowance* ran
// out, not that the request was malformed.
//
// D3 (fixed 2026-11-11): every provider funnelled all 400s into
// BadRequestError, so a gateway refusing further requests ("maximum provider
// request budget of N was reached") read as `bad request: {...}`. That string
// is not merely ugly — two behaviours keyed off it were wrong:
//
//   - the DAG step path treats BadRequestError as "this request will never
//     succeed, degrade gracefully" and SKIPS the step, silently dropping its
//     output, when in fact the step was fine and the run had merely spent its
//     allowance;
//   - retrying is pointless in a different way: the allowance does not grow
//     between attempts, so the step burned attempts against a wall.
//
// Evidence: 223 twice and 030 off r3 (2026-11-11), each looking like a broken
// mechanism rather than an exhausted budget.
type BudgetExhaustedError struct{ Msg string }

func (e *BudgetExhaustedError) Error() string {
	return "provider request budget exhausted: " + e.Msg
}

// String marker for the flattened form, so callers that only hold the message
// (step.LastError after SanitizeError) still classify it correctly — the same
// reason rate limits carry a string fallback in core/failure_classify.go.
const BudgetExhaustedMarker = "provider request budget exhausted"

type ProviderError struct{ Msg string }

func (e *ProviderError) Error() string { return "provider error: " + e.Msg }

// RateLimitedProvider wraps a provider with a proactive rate limiter.
type RateLimitedProvider struct {
	Base    interfaces.Provider
	Limiter ratelimit.RateLimiter
}

func (p *RateLimitedProvider) Name() string { return p.Base.Name() }

func (p *RateLimitedProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	if p.Limiter != nil {
		if err := p.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	return p.Base.Complete(ctx, req)
}

func (p *RateLimitedProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	if p.Limiter != nil {
		if err := p.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	return p.Base.StreamComplete(ctx, req, onChunk)
}

func (p *RateLimitedProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if p.Limiter != nil {
		if err := p.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	return p.Base.Embed(ctx, text)
}

func (p *RateLimitedProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return p.Base.CountTokens(ctx, text)
}

// Provider is the interface all AI backends must satisfy.
type Provider = interfaces.Provider
type CompleteRequest = schemas.CompleteRequest
type ProviderResponse = schemas.ProviderResponse
type ToolCall = schemas.ToolCall
type MCPServerDef = schemas.MCPServerDef
type Attachment = schemas.Attachment
