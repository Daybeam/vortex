package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/daybeam/vortex/config"
)

// ─── Ollama ───────────────────────────────────────────────────────────────

type OllamaProvider struct {
	cfg    *config.ProviderConfig
	name   string
	client *http.Client
}

func (p *OllamaProvider) Name() string { return p.name }

func (p *OllamaProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := onChunk(resp.Text); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *OllamaProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	baseURL := p.cfg.BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	// Use OpenAI-compatible endpoint
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
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

	respBody, status, headers, err := p.post(ctx, baseURL+"/chat/completions", body, map[string]string{
		"Authorization": "Bearer ollama",
		"Content-Type":  "application/json",
	})
	if err != nil {
		return nil, err
	}
	if status == 429 {
		return nil, &RateLimitError{Msg: string(respBody), RetryAfter: parseRetryAfter(headers)}
	}
	if status == 400 {
		return nil, &BadRequestError{Msg: string(respBody)}
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

func (p *OllamaProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	baseURL := p.cfg.BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	model := p.cfg.EmbeddingModel
	if model == "" {
		model = p.cfg.Model
	}

	body := map[string]any{
		"model":  model,
		"prompt": text,
	}

	respBody, status, headers, err := p.post(ctx, baseURL+"/api/embeddings", body, nil)
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
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, err
	}
	return resp.Embedding, nil
}
