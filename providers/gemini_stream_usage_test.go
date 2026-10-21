package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// staticBodyTransport answers every request with a canned response body. It
// exists because GeminiProvider's endpoint is a hardcoded Google URL that
// httptest cannot intercept; the client field is injectable, so substituting
// the transport is the only way to exercise the real SSE parsing loop offline.
type staticBodyTransport struct {
	status int
	body   string
}

func (t *staticBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Request:    req,
	}, nil
}

// geminiStreamUsageTest is the Gemini counterpart of §5.7. Before 2026-10-20
// the streaming path returned only Text+ToolCalls: it never parsed
// `usageMetadata` and never read `finishReason`, so every streaming Gemini call
// recorded zero tokens. Token accounting, the cache sentinel, the outer-ring
// context profile and the empty-response breaker all consume those fields.
//
// This test fails on the pre-fix code (PromptTokens/CompletionTokens == 0).
func TestGeminiStreamComplete_ParsesUsageAndFinishReason(t *testing.T) {
	// Real Gemini SSE shape: usageMetadata is TOP-LEVEL on the chunk (not inside
	// candidates), and the final frame carries totals with no parts.
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":" world"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1234,"candidatesTokenCount":56,"totalTokenCount":1290}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	p := &GeminiProvider{
		cfg:    &config.ProviderConfig{Provider: "gemini", Model: "gemini-2.0-flash"},
		name:   "gemini",
		client: &http.Client{Transport: &staticBodyTransport{status: 200, body: body}},
	}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gemini-2.0-flash", System: "s", User: "u", MaxTokens: 100,
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.Text != "Hello world" {
		t.Errorf("Text = %q, want %q", resp.Text, "Hello world")
	}
	if resp.PromptTokens != 1234 {
		t.Errorf("PromptTokens = %d, want 1234 (streaming usage was not parsed)", resp.PromptTokens)
	}
	if resp.CompletionTokens != 56 {
		t.Errorf("CompletionTokens = %d, want 56 (streaming usage was not parsed)", resp.CompletionTokens)
	}
	if resp.StopReason != "STOP" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "STOP")
	}
}

// The truncation signature for Gemini is finishReason MAX_TOKENS. Pin that the
// value survives into StopReason so the spawner's output_truncated event (which
// keys off it) can fire on this provider too.
func TestGeminiStreamComplete_MaxTokensFinishReason(t *testing.T) {
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"{\"steps\": [{\"id\": \"a"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":900,"candidatesTokenCount":8192,"totalTokenCount":9092}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	p := &GeminiProvider{
		cfg:    &config.ProviderConfig{Provider: "gemini", Model: "gemini-2.0-flash"},
		name:   "gemini",
		client: &http.Client{Transport: &staticBodyTransport{status: 200, body: body}},
	}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gemini-2.0-flash", System: "s", User: "u", MaxTokens: 8192,
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.StopReason != "MAX_TOKENS" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "MAX_TOKENS")
	}
	if resp.CompletionTokens != 8192 {
		t.Errorf("CompletionTokens = %d, want 8192", resp.CompletionTokens)
	}
}
