package providers

import (
	"context"
	"fmt"
	"strings"
)

// FallbackProvider wraps multiple providers and tries them sequentially.
type FallbackProvider struct {
	Providers []Provider
}

func (fp *FallbackProvider) Name() string {
	names := make([]string, len(fp.Providers))
	for i, p := range fp.Providers {
		names[i] = p.Name()
	}
	return "fallback(" + strings.Join(names, ", ") + ")"
}

func (fp *FallbackProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	var lastErr error
	for _, p := range fp.Providers {
		resp, err := p.StreamComplete(ctx, req, onChunk)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

func (fp *FallbackProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	var lastErr error
	for _, p := range fp.Providers {
		resp, err := p.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

func (fp *FallbackProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	var lastErr error
	for _, p := range fp.Providers {
		res, err := p.Embed(ctx, text)
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
