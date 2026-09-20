package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/pkg/safelimits"
)

// ─── Shared HTTP helper ────────────────────────────────────────────────────

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

var sanitizeKeyRe = regexp.MustCompile(`([?&](?:key|api_key|token)=)[^&\s"']+`)

func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	msg := sanitizeKeyRe.ReplaceAllString(err.Error(), "${1}***")
	return fmt.Errorf("%s", msg)
}

func doPost(ctx context.Context, client httpDoer, url string, body map[string]any, headers map[string]string) ([]byte, int, http.Header, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	if tid := observability.GetTraceID(ctx); tid != "" {
		req.Header.Set("x-trace-id", tid)
	}
	if sid := observability.GetSpanID(ctx); sid != "" {
		req.Header.Set("x-span-id", sid)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, sanitizeError(err)
	}
	defer resp.Body.Close()
	// audit H1: cap to prevent OOM from unbounded provider responses
	b, err := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	return b, resp.StatusCode, resp.Header, err
}

func parseRetryAfter(header http.Header) int {
	val := header.Get("Retry-After")
	if val == "" {
		return 0
	}
	// Try parsing as seconds
	if sec, err := strconv.Atoi(val); err == nil {
		return sec
	}
	// Try parsing as HTTP date
	if t, err := http.ParseTime(val); err == nil {
		sec := int(time.Until(t).Seconds())
		if sec < 0 {
			return 0
		}
		return sec
	}
	return 0
}

// Attach post method to each provider using the shared helper
func (p *AnthropicProvider) post(ctx context.Context, url string, body map[string]any, headers map[string]string) ([]byte, int, http.Header, error) {
	return doPost(ctx, p.client, url, body, headers)
}
func (p *OpenAIProvider) post(ctx context.Context, url string, body map[string]any, headers map[string]string) ([]byte, int, http.Header, error) {
	return doPost(ctx, p.client, url, body, headers)
}
func (p *GeminiProvider) post(ctx context.Context, url string, body map[string]any, headers map[string]string) ([]byte, int, http.Header, error) {
	return doPost(ctx, p.client, url, body, headers)
}
func (p *OllamaProvider) post(ctx context.Context, url string, body map[string]any, headers map[string]string) ([]byte, int, http.Header, error) {
	return doPost(ctx, p.client, url, body, headers)
}

func (p *AnthropicProvider) CountTokens(ctx context.Context, text string) (int, error) {
	// FIX (2026-07-08): previously a flat len(text)/4 heuristic. Anthropic
	// exposes a real /v1/messages/count_tokens endpoint (same request shape
	// as /v1/messages minus max_tokens); use it when a model is configured,
	// and fall back to the CJK-aware estimateTokens heuristic on any
	// transport/parse failure so a transient API hiccup can't hard-fail
	// core/context_manager.go's compression Audit() decision.
	model := p.cfg.Model
	if model == "" {
		return estimateTokens(text), nil
	}

	baseURL := p.cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": text}},
	}
	respBody, status, _, err := p.post(ctx, baseURL+"/v1/messages/count_tokens", body, map[string]string{
		"x-api-key":         p.cfg.ResolveAPIKey(nil),
		"anthropic-version": "2023-06-01",
		"content-type":      "application/json",
	})
	if err != nil || status >= 400 {
		return estimateTokens(text), nil
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil || resp.InputTokens == 0 {
		return estimateTokens(text), nil
	}
	return resp.InputTokens, nil
}

func (p *OpenAIProvider) CountTokens(ctx context.Context, text string) (int, error) {
	// FIX (2026-07-08): CJK-aware estimate instead of flat len(text)/4.
	// OpenAI doesn't expose a lightweight server-side token-counting
	// endpoint (tiktoken is normally used client-side, which would pull in
	// a real BPE tokenizer dependency); estimateTokens is the pragmatic
	// middle ground until that's worth the dependency weight.
	return estimateTokens(text), nil
}

func (p *GeminiProvider) CountTokens(ctx context.Context, text string) (int, error) {
	apiKey := p.cfg.ResolveAPIKey(nil)
	model := p.cfg.Model
	if after, ok := strings.CutPrefix(model, "models/"); ok {
		model = after
	}
	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:countTokens",
		model,
	)

	body := map[string]any{
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []map[string]any{
					{"text": text},
				},
			},
		},
	}

	respBody, status, _, err := p.post(ctx, url, body, map[string]string{
		"Content-Type":   "application/json",
		"x-goog-api-key": apiKey,
	})
	// FIX (2026-07-08): previously any transport/HTTP error here propagated
	// all the way up through core/context_manager.go's Audit(), which would
	// hard-fail the whole compression decision (and therefore, depending on
	// the caller, the task) over what's usually a transient token-counting
	// hiccup. Degrade to the CJK-aware estimateTokens heuristic instead.
	if err != nil || status >= 400 {
		return estimateTokens(text), nil
	}

	var resp struct {
		TotalTokens int `json:"totalTokens"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil || resp.TotalTokens == 0 {
		return estimateTokens(text), nil
	}
	return resp.TotalTokens, nil
}

func (p *OllamaProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return estimateTokens(text), nil
}

func (p *ChinaMobileProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return estimateTokens(text), nil
}

func (p *ScriptProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return estimateTokens(text), nil
}

func (p *ExternalScriptProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return estimateTokens(text), nil
}
