package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

// §5.7 regression (2026-10-20): OpenAIProvider did not parse `usage` at all,
// so every token figure on an OpenAI-compatible channel (including the AppWorld
// ModelProxy used by the eval harness) was 0. Cost tracking, the cache-efficiency
// sentinel and the outer-ring context accounting all read those fields, and the
// empty-response circuit breaker keys off CompletionTokens — so the breaker could
// never distinguish "no output" from "not measured".
//
// These tests fail on the pre-fix code because resp.Usage did not exist and the
// returns passed no PromptTokens/CompletionTokens/CacheReadTokens.

func TestOpenAIStreamComplete_ParsesUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		// The usage-bearing final frame carries NO choices. Before the fix,
		// `if len(chunk.Choices) == 0 { continue }` skipped it silently.
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":56,"prompt_tokens_details":{"cached_tokens":789}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "s", User: "u", MaxTokens: 100,
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.PromptTokens != 1234 {
		t.Errorf("PromptTokens = %d, want 1234", resp.PromptTokens)
	}
	if resp.CompletionTokens != 56 {
		t.Errorf("CompletionTokens = %d, want 56", resp.CompletionTokens)
	}
	if resp.CacheReadTokens != 789 {
		t.Errorf("CacheReadTokens = %d, want 789", resp.CacheReadTokens)
	}
}

func TestOpenAIComplete_ParsesUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":3}}}`)
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.Complete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "s", User: "u", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.PromptTokens != 10 || resp.CompletionTokens != 20 || resp.CacheReadTokens != 3 {
		t.Errorf("usage = (%d,%d,%d), want (10,20,3)",
			resp.PromptTokens, resp.CompletionTokens, resp.CacheReadTokens)
	}
}

// StopReason had NO reader anywhere in the tree before 2026-10-20. Without it,
// a response cut off by the output cap is indistinguishable from one where the
// model simply stopped — which is exactly the ambiguity that made the AppWorld
// 4fab96f_3 planner truncation take a full diagnosis cycle to attribute.

func TestOpenAIStreamComplete_CapturesFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// "" on intermediate frames, "length" on the closing frame — last
		// non-empty value must win.
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"partial out"},"finish_reason":""}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":8192}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "s", User: "u", MaxTokens: 8192,
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.StopReason != "length" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "length")
	}
	// The truncation signature we saw on AppWorld: cap hit AND completion
	// tokens equal to the cap. Both must be observable together.
	if resp.CompletionTokens != 8192 {
		t.Errorf("CompletionTokens = %d, want 8192", resp.CompletionTokens)
	}
}

func TestOpenAIComplete_CapturesFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.Complete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "s", User: "u", MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "stop")
	}
}

// The request must actually ask for usage on a streaming call, otherwise
// OpenAI-compatible servers never send the usage frame at all.
func TestOpenAIStreamComplete_RequestsIncludeUsage(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	if _, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "s", User: "u", MaxTokens: 10,
	}, func(string) error { return nil }); err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if !contains(gotBody, `"include_usage":true`) {
		t.Errorf("request body missing stream_options.include_usage; body=%s", gotBody)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
