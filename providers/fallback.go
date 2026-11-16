package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FallbackProvider wraps multiple providers and tries them sequentially.
type FallbackProvider struct {
	Providers []Provider
}

const (
	// fallbackPerProviderTimeout is the timeout given to each fallback provider
	// when the parent context is expired or nearly expired. Without this, every
	// fallback after a timeout fails instantly with the same expired context,
	// making the fallback chain dead code.
	fallbackPerProviderTimeout = 2 * time.Minute

	// fallbackMinRemaining is the minimum remaining time a fallback provider
	// needs to have a meaningful chance. If the parent context has less than
	// this remaining, the fallback gets its own fresh timeout instead.
	fallbackMinRemaining = 30 * time.Second
)

func (fp *FallbackProvider) Name() string {
	names := make([]string, len(fp.Providers))
	for i, p := range fp.Providers {
		names[i] = p.Name()
	}
	return "fallback(" + strings.Join(names, ", ") + ")"
}

// fallbackCtx ensures a fallback provider (index > 0) gets a meaningful
// timeout, even if the parent context expired or has very little time left.
//
// Without this, the expired parent context poisons all fallback attempts:
// every provider after the first fails instantly with context.DeadlineExceeded,
// making the fallback chain dead code when the primary provider times out.
//
// Returns a fresh context + cancel func when the parent context is
// insufficient, or the original context + nil when it has enough time.
// context.Canceled (user-initiated) is never extended — only
// context.DeadlineExceeded (turn budget timeout) gets a fresh window.
func fallbackCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) >= fallbackMinRemaining {
				return ctx, nil
			}
		} else {
			return ctx, nil
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx, nil
	}
	return context.WithTimeout(context.Background(), fallbackPerProviderTimeout)
}

func (fp *FallbackProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	var lastErr error
	for i, p := range fp.Providers {
		callCtx := ctx
		var cancel context.CancelFunc
		if i > 0 {
			callCtx, cancel = fallbackCtx(ctx)
		}
		resp, err := p.StreamComplete(callCtx, req, onChunk)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

func (fp *FallbackProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	var lastErr error
	for i, p := range fp.Providers {
		callCtx := ctx
		var cancel context.CancelFunc
		if i > 0 {
			callCtx, cancel = fallbackCtx(ctx)
		}
		resp, err := p.Complete(callCtx, req)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

func (fp *FallbackProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	var lastErr error
	for i, p := range fp.Providers {
		callCtx := ctx
		var cancel context.CancelFunc
		if i > 0 {
			callCtx, cancel = fallbackCtx(ctx)
		}
		res, err := p.Embed(callCtx, text)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return res, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

func (fp *FallbackProvider) CountTokens(ctx context.Context, text string) (int, error) {
	var lastErr error
	for _, p := range fp.Providers {
		count, err := p.CountTokens(ctx, text)
		if err == nil {
			return count, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("all providers failed, last error: %w", lastErr)
}
