package providers

import (
	"context"
	"fmt"
)

// ─── Host (Delegation) ───────────────────────────────────────────────────

type HostProvider struct {
	name string
}

func (p *HostProvider) Name() string { return p.name }

func (p *HostProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	return nil, &ProviderError{Msg: "DELEGATION_REQUIRED"}
}

func (p *HostProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	return nil, &ProviderError{Msg: "DELEGATION_REQUIRED"}
}

func (p *HostProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by HostProvider")
}

func (p *HostProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return estimateTokens(text), nil
}
