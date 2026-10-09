package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestRollingWindowMemory_ShortHistoryUnchanged(t *testing.T) {
	mem := NewRollingWindowMemory(10)
	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "Hello"},
		{ID: "2", Role: "assistant", Content: "Hi there!"},
	}

	got := mem.Process(context.Background(), "session1", history)
	if len(got) != 2 {
		t.Fatalf("Expected 2 messages for short history, got %d", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "2" {
		t.Errorf("Short history messages altered")
	}
}

func TestRollingWindowMemory_CompressesOlderHistory(t *testing.T) {
	mem := NewRollingWindowMemory(4)
	history := make([]ChatMessage, 12)
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history[i] = ChatMessage{
			ID:      fmt.Sprintf("msg_%d", i),
			Role:    role,
			Content: fmt.Sprintf("Turn %d message content", i),
		}
	}

	got := mem.Process(context.Background(), "session123", history)

	// Expected: 1 summary message + 4 recent messages = 5 total
	if len(got) != 5 {
		t.Fatalf("Expected 5 messages after compression, got %d", len(got))
	}

	if got[0].Role != "system" {
		t.Errorf("First message should be system summary, got %s", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "Prior Conversation Summary:") {
		t.Errorf("Summary header missing in first message")
	}

	// Recent 4 messages should match history[8..11]
	for i := 0; i < 4; i++ {
		if got[i+1].ID != fmt.Sprintf("msg_%d", i+8) {
			t.Errorf("Expected recent msg_%d at index %d, got %s", i+8, i+1, got[i+1].ID)
		}
	}
}

// ── FactCache Tests ──────────────────────────────────────────────

func TestFactCache_ExtractsPathsAndPreferences(t *testing.T) {
	fc := &FactCache{}
	history := []ChatMessage{
		{Role: "user", Content: "I prefer Python for the backend"},
		{Role: "assistant", Content: "Got it"},
		{Role: "user", Content: "Check the file at /home/user/project/main.go"},
	}

	result := fc.ExtractAndInject(history)
	if !strings.Contains(result, "prefers Python") {
		t.Errorf("expected 'prefers Python' in facts, got: %s", result)
	}
	if !strings.Contains(result, "/home/user/project/main.go") {
		t.Errorf("expected path in facts, got: %s", result)
	}
}

func TestFactCache_NoFactsReturnsEmpty(t *testing.T) {
	fc := &FactCache{}
	history := []ChatMessage{
		{Role: "user", Content: "Hello there"},
		{Role: "assistant", Content: "Hi"},
	}
	if result := fc.ExtractAndInject(history); result != "" {
		t.Errorf("expected empty string, got: %s", result)
	}
}

func TestFactCache_PersistsToStore(t *testing.T) {
	tmpDir := t.TempDir()
	s := store.NewMemoryBankStore(tmpDir, nil)
	fc := &FactCache{Store: s}

	history := []ChatMessage{
		{Role: "user", Content: "I prefer Go and using React"},
	}
	result := fc.ExtractAndInject(history)
	if !strings.Contains(result, "prefers Go") {
		t.Errorf("expected 'prefers Go', got: %s", result)
	}

	mb, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, p := range mb.UserProfile.Preferences {
		if strings.Contains(p, "prefers Go") {
			found = true
			break
		}
	}
	if !found {
		t.Error("fact not persisted to UserProfile.Preferences")
	}
}

// ── LLM SummaryModel Tests ──────────────────────────────────────

type summaryMockProvider struct {
	called bool
}

func (p *summaryMockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.called = true
	return &schemas.ProviderResponse{Text: "LLM summary: key decisions were made about config."}, nil
}

func (p *summaryMockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return p.Complete(ctx, req)
}

func (p *summaryMockProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (p *summaryMockProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *summaryMockProvider) Name() string                                               { return "summary-mock" }

func TestRollingWindowMemory_LLMSummary(t *testing.T) {
	mp := &summaryMockProvider{}
	mem := &RollingWindowMemory{
		WindowSize:   2,
		SummaryModel: mp,
	}
	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "Turn 1"},
		{ID: "2", Role: "assistant", Content: "Response 1"},
		{ID: "3", Role: "user", Content: "Turn 2"},
		{ID: "4", Role: "assistant", Content: "Response 2"},
	}

	got := mem.Process(context.Background(), "s", history)
	if !mp.called {
		t.Fatal("SummaryModel.Complete was not called")
	}
	if !strings.Contains(got[0].Content, "LLM summary") {
		t.Errorf("expected LLM summary in first message, got: %s", got[0].Content)
	}
}

type errorMockProvider struct{}

func (p *errorMockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return nil, fmt.Errorf("provider unavailable")
}
func (p *errorMockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return p.Complete(ctx, req)
}
func (p *errorMockProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (p *errorMockProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *errorMockProvider) Name() string                                               { return "error-mock" }

func TestRollingWindowMemory_LLMSummaryFallbackOnError(t *testing.T) {
	mem := &RollingWindowMemory{
		WindowSize:   2,
		SummaryModel: &errorMockProvider{},
	}
	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "Turn 1 content here"},
		{ID: "2", Role: "assistant", Content: "Response 1 content here"},
		{ID: "3", Role: "user", Content: "Turn 2 content here"},
		{ID: "4", Role: "assistant", Content: "Response 2 content here"},
	}

	got := mem.Process(context.Background(), "s", history)
	if !strings.Contains(got[0].Content, "Prior Conversation Summary:") {
		t.Errorf("expected fallback summary, got: %s", got[0].Content)
	}
}

// ── Token Budget Tests ──────────────────────────────────────────

func TestRollingWindowMemory_TokenBudgetTrims(t *testing.T) {
	mem := &RollingWindowMemory{
		WindowSize: 10,
		MaxTokens:  80,
	}
	history := make([]ChatMessage, 6)
	for i := 0; i < 6; i++ {
		history[i] = ChatMessage{
			ID:      fmt.Sprintf("msg_%d", i),
			Role:    "user",
			Content: strings.Repeat("x", 100),
		}
	}

	got := mem.Process(context.Background(), "s", history)
	totalTokens := 0
	for _, m := range got {
		totalTokens += len(m.Content) / 4
	}
	if totalTokens > 80 {
		t.Errorf("expected trimmed to <=80 tokens, got %d", totalTokens)
	}
	if len(got) < 1 {
		t.Errorf("expected at least 1 message after trimming, got %d", len(got))
	}
}

func TestRollingWindowMemory_TokenBudgetNoopWhenUnderLimit(t *testing.T) {
	mem := &RollingWindowMemory{
		WindowSize: 10,
		MaxTokens:  1000,
	}
	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "short"},
		{ID: "2", Role: "assistant", Content: "reply"},
	}
	got := mem.Process(context.Background(), "s", history)
	if len(got) != 2 {
		t.Errorf("expected 2 messages (no trimming), got %d", len(got))
	}
}

// ── All Layers Integration ──────────────────────────────────────

func TestRollingWindowMemory_AllLayers(t *testing.T) {
	mp := &summaryMockProvider{}
	mem := &RollingWindowMemory{
		WindowSize:   2,
		SummaryModel: mp,
		FactCache:    &FactCache{},
		MaxTokens:    500,
	}
	history := []ChatMessage{
		{ID: "1", Role: "user", Content: "I prefer Rust for systems programming"},
		{ID: "2", Role: "assistant", Content: "Noted your preference for Rust"},
		{ID: "3", Role: "user", Content: "Now check /etc/config/server.toml"},
		{ID: "4", Role: "assistant", Content: "Config file read successfully"},
	}

	got := mem.Process(context.Background(), "s", history)

	hasSummary := false
	hasFacts := false
	for _, m := range got {
		if strings.Contains(m.Content, "Prior Conversation Summary:") {
			hasSummary = true
		}
		if strings.Contains(m.Content, "Durable facts:") {
			hasFacts = true
		}
	}
	if !hasSummary {
		t.Error("missing LLM summary layer")
	}
	if !hasFacts {
		t.Error("missing fact cache layer")
	}
}
