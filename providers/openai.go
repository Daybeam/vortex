package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/pkg/safelimits"
)

// getOpenAIEndpoint normalizes OpenAI-compatible base URLs and appends a path suffix.
// It is tolerant of base URLs that already end with /v1.
func getOpenAIEndpoint(baseURL, suffix string) string {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL + suffix
}

// ─── OpenAI ───────────────────────────────────────────────────────────────

type OpenAIProvider struct {
	cfg    *config.ProviderConfig
	name   string
	client *http.Client
}

func (p *OpenAIProvider) Name() string { return p.name }

func (p *OpenAIProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	chatURL := getOpenAIEndpoint(p.cfg.BaseURL, "/v1/chat/completions")

	userContent := any(req.User)
	if len(req.Attachments) > 0 {
		parts := []map[string]any{
			{"type": "text", "text": req.User},
		}
		for _, att := range req.Attachments {
			data, err := os.ReadFile(att.Path)
			if err != nil {
				log.Printf("WARN: openai_provider: failed to read attachment %s: %v", att.Path, err)
				continue
			}
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": fmt.Sprintf("data:%s;base64,%s", att.MimeType, base64.StdEncoding.EncodeToString(data)),
				},
			})
		}
		userContent = parts
	}

	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     true,
		"messages": []map[string]any{
			{"role": "system", "content": req.System},
			{"role": "user", "content": userContent},
		},
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.FrequencyPenalty != nil {
		body["frequency_penalty"] = *req.FrequencyPenalty
	}

	if len(req.MCPServers) > 0 {
		var tools []map[string]any
		seen := make(map[string]bool)
		for _, s := range req.MCPServers {
			for _, t := range s.Tools {
				if seen[t.Name] {
					continue
				}
				seen[t.Name] = true
				tools = append(tools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        t.Name,
						"description": t.Description,
						"parameters":  t.InputSchema,
					},
				})
			}
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.ResolveAPIKey(req.Secrets))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if tid := observability.GetTraceID(ctx); tid != "" {
		httpReq.Header.Set("x-trace-id", tid)
	}
	if sid := observability.GetSpanID(ctx); sid != "" {
		httpReq.Header.Set("x-span-id", sid)
	}

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, sanitizeError(err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == 429 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, safelimits.MaxErrorBody)) // audit H1: cap error body at 1MB
		return nil, &RateLimitError{Msg: string(b), RetryAfter: parseRetryAfter(httpResp.Header)}
	}
	if httpResp.StatusCode == 400 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, safelimits.MaxErrorBody)) // audit H1
		return nil, &BadRequestError{Msg: string(b)}
	}
	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, safelimits.MaxErrorBody)) // audit H1
		return nil, &ProviderError{Msg: fmt.Sprintf("HTTP %d: %s", httpResp.StatusCode, b)}
	}

	var fullText strings.Builder
	toolCallMap := make(map[int]*streamingToolCall)

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			fullText.WriteString(delta.Content)
			if onChunk != nil {
				if err := onChunk(delta.Content); err != nil {
					return nil, err
				}
			}
		}
		for _, tc := range delta.ToolCalls {
			entry := toolCallMap[tc.Index]
			if entry == nil {
				entry = &streamingToolCall{}
				toolCallMap[tc.Index] = entry
			}
			if tc.ID != "" {
				entry.callID = tc.ID
			}
			if tc.Function.Name != "" {
				entry.name = tc.Function.Name
			}
			entry.argsBuf.WriteString(tc.Function.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		// audit H6: return both partial text and error so callers can detect
		// truncation instead of silently treating partial output as complete.
		return &ProviderResponse{Text: fullText.String()}, &ProviderError{Msg: "stream read error (partial result): " + err.Error()}
	}

	var toolCalls []ToolCall
	for i := 0; i < len(toolCallMap); i++ {
		entry := toolCallMap[i]
		if entry == nil || entry.name == "" {
			continue
		}
		var args map[string]any
		if entry.argsBuf.Len() > 0 {
			if err := json.Unmarshal([]byte(entry.argsBuf.String()), &args); err != nil {
				log.Printf("WARN: openai_provider: malformed tool call args for %s: %v", entry.name, err)
			}
		}
		toolCalls = append(toolCalls, ToolCall{
			CallID:    entry.callID,
			Name:      entry.name,
			Arguments: args,
		})
	}

	text := fullText.String()
	if text == "" && len(toolCalls) == 0 {
		log.Printf("WARN: openai_provider: streaming returned empty response, falling back to non-streaming Complete")
		return p.Complete(ctx, req)
	}

	return &ProviderResponse{Text: text, ToolCalls: toolCalls}, nil
}

type streamingToolCall struct {
	callID  string
	name    string
	argsBuf strings.Builder
}

func (p *OpenAIProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	chatURL := getOpenAIEndpoint(p.cfg.BaseURL, "/v1/chat/completions")

	userContent := any(req.User)
	if len(req.Attachments) > 0 {
		parts := []map[string]any{
			{"type": "text", "text": req.User},
		}
		for _, att := range req.Attachments {
			data, err := os.ReadFile(att.Path)
			if err != nil {
				log.Printf("WARN: openai_provider: failed to read attachment %s: %v", att.Path, err)
				continue
			}
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": fmt.Sprintf("data:%s;base64,%s", att.MimeType, base64.StdEncoding.EncodeToString(data)),
				},
			})
		}
		userContent = parts
	}

	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages": []map[string]any{
			{"role": "system", "content": req.System},
			{"role": "user", "content": userContent},
		},
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.FrequencyPenalty != nil {
		body["frequency_penalty"] = *req.FrequencyPenalty
	}

	// Native Tools Support
	if len(req.MCPServers) > 0 {
		var tools []map[string]any
		seen := make(map[string]bool) // FIX (2026-07-14): re-apply F8 dedup (regressed) - avoid duplicate tool names across MCPs
		for _, s := range req.MCPServers {
			for _, t := range s.Tools {
				if seen[t.Name] {
					continue
				}
				seen[t.Name] = true
				tools = append(tools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        t.Name,
						"description": t.Description,
						"parameters":  t.InputSchema,
					},
				})
			}
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}

	respBody, status, headers, err := p.post(ctx, chatURL, body, map[string]string{
		"Authorization": "Bearer " + p.cfg.ResolveAPIKey(req.Secrets),
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
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &ProviderError{Msg: "failed to parse response: " + err.Error()}
	}
	if len(resp.Choices) == 0 {
		return nil, &ProviderError{Msg: "no choices in response"}
	}

	msg := resp.Choices[0].Message
	var toolCalls []ToolCall
	for _, tc := range msg.ToolCalls {
		if tc.Type == "function" {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			toolCalls = append(toolCalls, ToolCall{
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
	}

	return &ProviderResponse{
		Text:      msg.Content,
		ToolCalls: toolCalls,
	}, nil
}

func (p *OpenAIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	embedURL := getOpenAIEndpoint(p.cfg.BaseURL, "/v1/embeddings")

	model := p.cfg.EmbeddingModel
	if model == "" {
		model = "text-embedding-3-small"
	}

	body := map[string]any{
		"model": model,
		"input": text,
	}

	respBody, status, headers, err := p.post(ctx, embedURL, body, map[string]string{
		"Authorization": "Bearer " + p.cfg.ResolveAPIKey(nil),
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
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &ProviderError{Msg: "failed to parse response: " + err.Error()}
	}
	if len(resp.Data) == 0 {
		return nil, &ProviderError{Msg: "no embedding in response"}
	}
	return resp.Data[0].Embedding, nil
}
