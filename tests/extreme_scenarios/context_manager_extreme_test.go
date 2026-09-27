package extreme_tests

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// mockProvider is a minimal Provider for ContextManager testing.
// It implements providers.Provider (= interfaces.Provider).
type mockProvider struct {
	name string
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: "mock"}, nil
}
func (m *mockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(text string) error) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: "mock"}, nil
}
func (m *mockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *mockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return len(text) / 4, nil // rough estimate: 4 chars per token
}

// Compile-time interface check.
var _ providers.Provider = (*mockProvider)(nil)

// TestExtreme_ContextManager_SuperLargeContext verifies that a context
// exceeding the model's hard limit triggers LevelCritical (Fork).
//
// Extreme scenario: Multi-step task accumulates 1M+ tokens of context.
func TestExtreme_ContextManager_SuperLargeContext(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test-provider",
		Providers: map[string]*config.ProviderConfig{
			"test-provider": {
				Provider:               "openai",
				Model:                  "gpt-4o",
				MaxContextWindow:       128000,
				EffectiveContextWindow: 100000,
				TokenLimit:             120000,
			},
		},
	}

	cm := core.NewContextManager(reg)

	// Simulate 1M tokens of volatile content (tool outputs, raw logs).
	// At ~4 chars/token, 1M tokens ≈ 4M chars. Use "token " (6 chars) → ~667K tokens.
	hugeContent := strings.Repeat("token ", 1_000_000) // ~6MB, ~1M tokens

	req := &schemas.CompleteRequest{
		Model:  "gpt-4o",
		System: "You are a helpful assistant.",
		User:   hugeContent,
	}

	provider := &mockProvider{name: "openai"}
	result, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}

	if result.Decision != core.LevelCritical {
		t.Errorf("expected LevelCritical for 1M tokens, got %v (action: %s)",
			result.Decision, result.ActionTaken)
	}
}

// TestExtreme_ContextManager_ProtectedZoneOverrun verifies that when
// protected content (SOP rules, system prompt) alone exceeds 75% of
// maxCtx, the system forces Fork (no compression can help).
//
// Extreme scenario: System prompt + SOP rules are so large they fill
// 80% of the context window before any user content is added.
func TestExtreme_ContextManager_ProtectedZoneOverrun(t *testing.T) {
	maxCtx := 100000
	reg := &config.Registry{
		DefaultProvider: "test-provider",
		Providers: map[string]*config.ProviderConfig{
			"test-provider": {
				Provider:               "openai",
				Model:                  "gpt-4o",
				MaxContextWindow:       maxCtx,
				EffectiveContextWindow: 80000,
			},
		},
	}

	cm := core.NewContextManager(reg)

	// Protected zone = ProtectedSystemBlocks (NEVER compressed). Make it 80% of maxCtx.
	// At ~4 chars/token, 80% of 100K tokens = 80K tokens = 320K chars.
	// "SOP rule: " = 10 chars → 32000 reps = 320K chars ≈ 80K tokens.
	hugeSystemPrompt := strings.Repeat("SOP rule: ", 32000) // ~320K chars ≈ 80K tokens

	req := &schemas.CompleteRequest{
		Model: "gpt-4o",
		ProtectedSystemBlocks: []schemas.ContentBlock{
			{Type: "text", Text: hugeSystemPrompt},
		},
		User: "small task",
	}

	provider := &mockProvider{name: "openai"}
	result, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}

	if result.ActionTaken != "PROTECTED_ZONE_OVERRUN_FORK" {
		t.Errorf("expected PROTECTED_ZONE_OVERRUN_FORK, got %s (decision: %v)",
			result.ActionTaken, result.Decision)
	}
}

// TestExtreme_ContextManager_GradualContextGrowth verifies that
// increasing context size transitions through compression levels.
//
// Extreme scenario: Context grows gradually during a long multi-step task.
//
// T-C15 fix: previously this test only logged results with t.Logf without
// any assertions. Now it asserts that (1) small context is not critical,
// (2) huge context triggers critical, and (3) decision level is
// monotonically non-decreasing as context size grows.
func TestExtreme_ContextManager_GradualContextGrowth(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test-provider",
		Providers: map[string]*config.ProviderConfig{
			"test-provider": {
				Provider:               "openai",
				Model:                  "gpt-4o",
				MaxContextWindow:       10000,
				EffectiveContextWindow: 8000,
			},
		},
	}

	cm := core.NewContextManager(reg)
	provider := &mockProvider{name: "openai"}

	sizes := []int{100, 1000, 5000, 10000, 50000}
	var prevLevel core.CompressionLevel = core.LevelNone
	for idx, size := range sizes {
		content := strings.Repeat("word ", size)
		req := &schemas.CompleteRequest{
			Model: "gpt-4o",
			User:  content,
		}

		result, err := cm.Audit(context.Background(), provider, req)
		if err != nil {
			t.Errorf("size %d: Audit failed: %v", size, err)
			continue
		}

		t.Logf("size=%d chars → decision=%v, action=%s, originalTokens=%d",
			size, result.Decision, result.ActionTaken, result.OriginalTokens)

		// Assertion 1: smallest context must NOT be critical
		if idx == 0 && result.Decision == core.LevelCritical {
			t.Errorf("size %d: expected non-critical for small context, got LevelCritical (%s)", size, result.ActionTaken)
		}

		// Assertion 2: largest context MUST be critical
		if idx == len(sizes)-1 && result.Decision != core.LevelCritical {
			t.Errorf("size %d: expected LevelCritical for huge context, got %v (%s)", size, result.Decision, result.ActionTaken)
		}

		// Assertion 3: decision level must be monotonically non-decreasing
		if result.Decision < prevLevel {
			t.Errorf("size %d: decision level went DOWN from %v to %v (non-monotonic)", size, prevLevel, result.Decision)
		}
		prevLevel = result.Decision
	}
}
