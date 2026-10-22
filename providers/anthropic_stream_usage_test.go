package providers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

// anthropicStreamUsageTest is the Anthropic counterpart of §5.7.
//
// Before 2026-10-20 the streaming path read ONLY the two cache counters off
// `message_delta.usage` and ignored input_tokens/output_tokens entirely. The
// result was a self-contradicting record — cache_read > prompt — because prompt
// was always 0. The cache sentinel compares prompt against cache_read, so it was
// comparing against a phantom.
//
// Anthropic reports input_tokens on message_start and output_tokens on
// message_delta, which is why both event types are handled.
func TestAnthropicStreamComplete_ParsesInputOutputTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		events := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":1500,"output_tokens":1,"cache_creation_input_tokens":200,"cache_read_input_tokens":800}}}`,
			`{"type":"content_block_start","content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop"}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
			`{"type":"message_stop"}`,
		}
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	resp, err := p.StreamComplete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 100,
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.PromptTokens != 1500 {
		t.Errorf("PromptTokens = %d, want 1500 (input_tokens not read from message_start)", resp.PromptTokens)
	}
	if resp.CompletionTokens != 42 {
		t.Errorf("CompletionTokens = %d, want 42 (output_tokens not read from message_delta)", resp.CompletionTokens)
	}
	// The cache counters were already working; they must not regress.
	if resp.CacheWriteTokens != 200 {
		t.Errorf("CacheWriteTokens = %d, want 200", resp.CacheWriteTokens)
	}
	if resp.CacheReadTokens != 800 {
		t.Errorf("CacheReadTokens = %d, want 800", resp.CacheReadTokens)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
	// The invariant that used to be violated: cached prefix cannot exceed the
	// total prompt, which is what surfaced the bug.
	if resp.CacheReadTokens > resp.PromptTokens {
		t.Errorf("cache_read (%d) > prompt (%d): prompt accounting is still broken",
			resp.CacheReadTokens, resp.PromptTokens)
	}
}

// A message_delta carrying only output_tokens must not wipe the input_tokens
// captured at message_start (Anthropic omits fields it is not updating).
func TestAnthropicStreamComplete_PartialUsageDoesNotZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		events := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":900}}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":8192}}`,
		}
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Provider: "anthropic", Model: "claude-3", BaseURL: server.URL},
		name:   "anthropic",
		client: server.Client(),
	}

	resp, err := p.StreamComplete(t.Context(), CompleteRequest{
		Model: "claude-3", System: "s", User: "u", MaxTokens: 8192,
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.PromptTokens != 900 {
		t.Errorf("PromptTokens = %d, want 900 (overwritten by a usage frame that omitted it)", resp.PromptTokens)
	}
	if resp.CompletionTokens != 8192 {
		t.Errorf("CompletionTokens = %d, want 8192", resp.CompletionTokens)
	}
	if resp.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "max_tokens")
	}
}
