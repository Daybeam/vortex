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
