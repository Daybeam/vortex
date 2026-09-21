package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// MockProvider implements providers.Provider for testing
type MockProvider struct {
	name string
}

func (m *MockProvider) Name() string { return m.name }
func (m *MockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	// Simple heuristic for testing: 1 token per 4 chars
	return len(text) / 4, nil
}
func (m *MockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: "test"}, nil
}
func (m *MockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return nil, nil
}
func (m *MockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func TestSievePipeline(t *testing.T) {
	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
	}
	reg.Providers["gemini"] = &config.ProviderConfig{
		Provider:               "gemini",
		MaxContextWindow:       1000, // Low limit for testing
		EffectiveContextWindow: 500,
		TokenLimit:             1200,
	}

	cm := NewContextManager(reg)
	provider := &MockProvider{name: "gemini"}

	// 1. Construct a "Dirty" and "Overloaded" context
	systemPrompt := "You are an S2T Agent. Goal: Analyze NVDA. " + strings.Repeat("System State Snapshot: OK\n", 20)
	userPrompt := "User: Start analysis\n" +
		strings.Repeat("[2026-07-08 12:00:00] Processing data... Progress: 10%\n", 50) +
		"Assistant: I found some data.\n" +
		strings.Repeat("[2026-07-08 12:00:01] Fetching from API... Progress: 20%\n", 50) +
		"User: Continue\n" +
		"Assistant: Final Result: NVDA is Bullish."

	req := &schemas.CompleteRequest{
		System: systemPrompt,
		User:   userPrompt,
	}

	// 2. Audit
	res, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	fmt.Printf("Audit Decision: %v, Original Tokens: %d, Action: %s\n", res.Decision, res.OriginalTokens, res.ActionTaken)

	// 3. Squeeze
	squeezedReq, squeezeRes, err := cm.Squeeze(context.Background(), provider, req, res.Decision)
	if err != nil {
		t.Fatalf("Squeeze failed: %v", err)
	}
	fmt.Printf("Squeeze Result: Compressed Tokens: %d, Ratio: %.2f\n", squeezeRes.CompressedTokens, squeezeRes.CompressionRatio)

	// 4. Semantic Verification
	if !strings.Contains(squeezedReq.System, "Goal: Analyze NVDA") {
		t.Errorf("Critical goal lost in system prompt")
	}
	if !strings.Contains(squeezedReq.User, "Final Result: NVDA is Bullish") {
		t.Errorf("Critical result lost in user prompt")
	}
	if strings.Contains(squeezedReq.User, "Progress: 10%") {
		t.Errorf("Noise not removed by RTK-Filter")
	}
}

func TestAudit_UnconfiguredContextWindow_DoesNotFalsePositiveCritical(t *testing.T) {
	// Regression test for the 2026-07-12 fix: a provider with no
	// max_context_window/effective_context_window/token_limit configured
	// (the common case for most providers in config_windows.json -- gemini,
	// gemma, openai, deepseek-v4-flash, sensenova-*, local/ollama) must not
	// be treated as having a hard limit of 0 tokens. Before the fix, this
	// exact setup made Audit() classify every non-empty prompt as
	// LevelCritical/HARD_LIMIT_BREACH_FORK on turn 0, which cascaded into an
	// unconditional DYNAMIC_ESCALATION_REQUIRED failure for every task using
	// an unconfigured provider.
	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
	}
	reg.Providers["gemini"] = &config.ProviderConfig{
		Provider: "gemini",
		// Deliberately NOT setting MaxContextWindow/EffectiveContextWindow/
		// TokenLimit, matching config_windows.json's actual "gemini" entry.
	}

	cm := NewContextManager(reg)
	provider := &MockProvider{name: "gemini"}

	req := &schemas.CompleteRequest{
		System: "You are a helpful assistant.",
		User:   "Please do a short, ordinary task.",
	}

	res, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelNone {
		t.Fatalf("expected LevelNone for an unconfigured context window, got %v (action: %s)", res.Decision, res.ActionTaken)
	}
}

func TestAudit_BrandingMatch(t *testing.T) {
	// Verify that Audit correctly matches the provider by its brand name
	// (e.g. "sensenova") rather than the technical implementation name ("openai").
	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
	}
	reg.Providers["sensenova-6.7-flash"] = &config.ProviderConfig{
		Provider:               "sensenova",
		MaxContextWindow:       1000,
		EffectiveContextWindow: 500,
		TokenLimit:             1200,
	}

	cm := NewContextManager(reg)
	provider := &MockProvider{name: "sensenova"}

	req := &schemas.CompleteRequest{
		System: "You are a helpful assistant.",
		User:   "Short task.",
	}

	res, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken == "NO_LIMIT_CONFIGURED" {
		t.Errorf("Audit failed to match provider branding; got NO_LIMIT_CONFIGURED")
	}
}

func TestApplyRTK(t *testing.T) {
	input := "[2026-07-08 12:00:00] Start\nProgress: 50%\nProcessing something... 100%\nUpdating database...\nKeep this line."
	got := applyRTK(input)
	if strings.Contains(got, "[2026-07-08") {
		t.Error("Timestamp not removed")
	}
	if strings.Contains(got, "Progress:") {
		t.Error("Progress log not removed")
	}
	if strings.Contains(got, "Processing") {
		t.Error("Processing log not removed")
	}
	if !strings.Contains(got, "Keep this line.") {
		t.Error("Valid content lost")
	}
}

func TestApplySessionDedup(t *testing.T) {
	longLine := strings.Repeat("A", 150)
	input := longLine + "\n" + longLine + "\nShort line.\n" + longLine
	got := applySessionDedup(input)
	count := strings.Count(got, "AAAA")
	if count > 150 { // Should only have one occurrence of longLine
		t.Errorf("Deduplication failed, found %d occurrences of long block", count/150)
	}
	if !strings.Contains(got, "Short line.") {
		t.Error("Short line lost")
	}
}

func TestApplySemanticPruning(t *testing.T) {
	input := "User: Task 1\nLine 2\nLine 3\nAssistant: OK\nUser: Task 2\nLine 2\nLine 3\nUser: Task 3\nAssistant: OK 3\nUser: Task 4\nAssistant: OK 4\nUser: Task 5\nAssistant: Final result."
	got := applySemanticPruning(input)
	if !strings.Contains(got, "Task 1") {
		t.Error("Head (Task 1) lost")
	}
	if !strings.Contains(got, "Final result.") {
		t.Error("Tail lost")
	}
	if !strings.Contains(got, "... [compressed]") {
		t.Error("Middle not compressed")
	}
}
