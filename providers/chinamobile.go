package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/daybeam/vortex/config"
)

// ─── ChinaMobile ────────────────────────────────────────────────────────────

type ChinaMobileProvider struct {
	cfg    *config.ProviderConfig
	name   string
	client *http.Client
}

func (p *ChinaMobileProvider) Name() string { return p.name }

func (p *ChinaMobileProvider) post(ctx context.Context, url string, body map[string]any, headers map[string]string) ([]byte, int, http.Header, error) {
	return doPost(ctx, p.client, url, body, headers)
}

func (p *ChinaMobileProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := onChunk(resp.Text); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *ChinaMobileProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	url := p.cfg.BaseURL + "/chat/completions"

	body := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.FrequencyPenalty != nil {
		body["frequency_penalty"] = *req.FrequencyPenalty
	}

	respBody, status, headers, err := p.post(ctx, url, body, map[string]string{
		"Authorization": "Bearer " + p.cfg.ResolveAPIKey(req.Secrets),
		"Content-Type":  "application/json",
	})
	if err != nil {
		return nil, err
	}
	if status == 429 {
		return nil, &RateLimitError{Msg: string(respBody), RetryAfter: parseRetryAfter(headers)}
	}
	if status >= 400 {
		return nil, &ProviderError{Msg: fmt.Sprintf("HTTP %d: %s", status, respBody)}
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &ProviderError{Msg: "failed to parse response: " + err.Error()}
	}
	if len(resp.Choices) == 0 {
		return nil, &ProviderError{Msg: "no choices in response"}
	}

	return &ProviderResponse{
		Text: resp.Choices[0].Message.Content,
	}, nil
}

func (p *ChinaMobileProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by ChinaMobileProvider")
}
