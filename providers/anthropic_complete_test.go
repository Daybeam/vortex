package providers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestAnthropicComplete_ParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"content": [{"type": "text", "text": "Hello world"}],
			"stop_reason": "end_turn",
			"usage": {
				"input_tokens": 100,
				"output_tokens": 20,
				"cache_creation_input_tokens": 10,
				"cache_read_input_tokens": 50
			}
		}`)
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	resp, err := p.Complete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Text != "Hello world" {
		t.Errorf("Text = %q, want %q", resp.Text, "Hello world")
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
	if resp.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", resp.PromptTokens)
	}
	if resp.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, want 20", resp.CompletionTokens)
	}
	if resp.CacheWriteTokens != 10 {
		t.Errorf("CacheWriteTokens = %d, want 10", resp.CacheWriteTokens)
	}
	if resp.CacheReadTokens != 50 {
		t.Errorf("CacheReadTokens = %d, want 50", resp.CacheReadTokens)
	}
}

func TestAnthropicComplete_ParsesToolUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"content": [
				{"type": "text", "text": "Let me search"},
				{"type": "tool_use", "id": "tool_1", "name": "search", "input": {"query": "go tests"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 50, "output_tokens": 10}
		}`)
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	resp, err := p.Complete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.CallID != "tool_1" {
		t.Errorf("CallID = %q, want %q", tc.CallID, "tool_1")
	}
	if tc.Name != "search" {
		t.Errorf("Name = %q, want %q", tc.Name, "search")
	}
	if tc.Arguments["query"] != "go tests" {
		t.Errorf("Arguments[query] = %v, want %q", tc.Arguments["query"], "go tests")
	}
}

func TestAnthropicComplete_RateLimitError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error": "rate limited"}`)
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	_, err := p.Complete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
	rle, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("expected *RateLimitError, got %T", err)
	}
	if rle.RetryAfter != 30 {
		t.Errorf("RetryAfter = %d, want 30", rle.RetryAfter)
	}
}

func TestAnthropicComplete_BadRequestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error": {"type": "invalid_request_error", "message": "bad model"}}`)
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	_, err := p.Complete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected error for 400, got nil")
	}
}

func TestAnthropicComplete_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error": "internal server error"}`)
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	_, err := p.Complete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	pe, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Msg == "" {
		t.Error("expected non-empty error message")
	}
}

func TestAnthropicEmbed_NotSupported(t *testing.T) {
	p := &AnthropicProvider{
		cfg:  &config.ProviderConfig{Provider: "anthropic"},
		name: "anthropic",
	}
	_, err := p.Embed(t.Context(), "test text")
	if err == nil {
		t.Fatal("expected error for Anthropic Embed, got nil")
	}
}
