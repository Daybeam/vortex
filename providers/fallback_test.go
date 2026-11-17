package providers

import (
	"context"
	"errors"
	"testing"
	"time"
)

type MockProvider struct {
	name    string
	fail    bool
	success bool
}

func (m *MockProvider) Name() string { return m.name }
func (m *MockProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if m.fail {
		return nil, errors.New("fail")
	}
	onChunk("success from " + m.name)
	return &ProviderResponse{Text: "success from " + m.name}, nil
}
func (m *MockProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
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

// ctxBlockingProvider blocks until ctx is done, then returns ctx.Err().
// Simulates a primary provider that outlasts the turn budget.
type ctxBlockingProvider struct{ name string }

func (p *ctxBlockingProvider) Name() string { return p.name }
func (p *ctxBlockingProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *ctxBlockingProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *ctxBlockingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *ctxBlockingProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}

// TestFallbackProvider_ExpiredCtxTriesFallback is the regression test for the
// bug where FallbackProvider passed the same expired ctx to all providers,
// making the fallback chain dead code when the primary provider timed out.
//
// Before the fix: primary blocks until ctx expires → fallback also gets the
// expired ctx → fallback fails instantly → "all providers failed".
// After the fix: primary blocks until ctx expires → fallback gets a fresh
// ctx with its own timeout → fallback succeeds.
func TestFallbackProvider_ExpiredCtxTriesFallback(t *testing.T) {
	primary := &ctxBlockingProvider{name: "primary"}
	fallback := &MockProvider{name: "fallback", success: true}
	fp := &FallbackProvider{Providers: []Provider{primary, fallback}}

	// Parent context with a short timeout — will expire while primary blocks.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp, err := fp.Complete(ctx, CompleteRequest{})
	if err != nil {
		t.Fatalf("expected fallback to succeed after primary timed out, got: %v", err)
	}
	if resp.Text != "success from fallback" {
		t.Fatalf("expected success from fallback, got: %s", resp.Text)
	}
}

// TestFallbackProvider_StreamExpiredCtxTriesFallback is the streaming variant
// of the above regression test.
func TestFallbackProvider_StreamExpiredCtxTriesFallback(t *testing.T) {
	primary := &ctxBlockingProvider{name: "primary"}
	fallback := &MockProvider{name: "fallback", success: true}
	fp := &FallbackProvider{Providers: []Provider{primary, fallback}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var chunks []string
	resp, err := fp.StreamComplete(ctx, CompleteRequest{}, func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed after primary timed out, got: %v", err)
	}
	if resp.Text != "success from fallback" {
		t.Fatalf("expected success from fallback, got: %s", resp.Text)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk from fallback")
	}
}

// TestFallbackProvider_CanceledCtxDoesNotExtend verifies that a user-initiated
// cancellation (context.Canceled) is NOT extended — only turn-budget timeouts
// (context.DeadlineExceeded) get a fresh window for fallbacks.
func TestFallbackProvider_CanceledCtxDoesNotExtend(t *testing.T) {
	primary := &ctxBlockingProvider{name: "primary"}
	fallback := &MockProvider{name: "fallback", success: true}
	fp := &FallbackProvider{Providers: []Provider{primary, fallback}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately — simulates user disconnect.
	cancel()

	_, err := fp.Complete(ctx, CompleteRequest{})
	if err == nil {
		t.Fatal("expected error when context is cancelled, not a successful fallback")
	}
}

// TestFallbackCtx_EnoughRemaining verifies that when the parent context has
// enough time remaining, the fallback uses the parent context as-is (no fresh
// context created).
func TestFallbackCtx_EnoughRemaining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	gotCtx, gotCancel := fallbackCtx(ctx)
	if gotCancel != nil {
		t.Fatal("expected nil cancel func when parent has enough time")
	}
	if gotCtx != ctx {
		t.Fatal("expected same context when parent has enough time")
	}
}

// TestFallbackCtx_NoDeadline verifies that a context without a deadline is
// passed through as-is.
func TestFallbackCtx_NoDeadline(t *testing.T) {
	ctx := context.Background()
	gotCtx, gotCancel := fallbackCtx(ctx)
	if gotCancel != nil {
		t.Fatal("expected nil cancel func for context without deadline")
	}
	if gotCtx != ctx {
		t.Fatal("expected same context for context without deadline")
	}
}
