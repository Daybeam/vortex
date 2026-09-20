package providers

import (
	"context"
	"errors"
	"testing"
)

type MockProvider struct {
	name    string
	fail    bool
	success bool
}

func (m *MockProvider) Name() string { return m.name }
func (m *MockProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	if m.fail {
		return nil, errors.New("fail")
	}
	onChunk("success from " + m.name)
	return &ProviderResponse{Text: "success from " + m.name}, nil
}
func (m *MockProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	if m.fail {
		return nil, errors.New("fail")
	}
	return &ProviderResponse{Text: "success from " + m.name}, nil
}
func (m *MockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *MockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func TestFallbackProvider(t *testing.T) {
	p1 := &MockProvider{name: "p1", fail: true}
	p2 := &MockProvider{name: "p2", fail: false}
	fp := &FallbackProvider{Providers: []Provider{p1, p2}}

	resp, err := fp.Complete(context.Background(), CompleteRequest{})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Text != "success from p2" {
		t.Fatalf("expected success from p2, got %s", resp.Text)
	}
}
