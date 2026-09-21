package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/pkg/safelimits"
)

// ─── Anthropic ───────────────────────────────────────────────────────────────

type AnthropicProvider struct {
	cfg    *config.ProviderConfig
	name   string
	client *http.Client
}

func (p *AnthropicProvider) Name() string { return p.name }

func (p *AnthropicProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	baseURL := p.cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     true,
	}

	// Handle block-based content for caching (Anthropic)
	if len(req.SystemBlocks) > 0 {
		blocks := make([]map[string]any, len(req.SystemBlocks))
		for i, b := range req.SystemBlocks {
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != "" {
				block["cache_control"] = map[string]string{"type": b.CacheControl}
			}
			blocks[i] = block
		}
		body["system"] = blocks
	} else {
		body["system"] = req.System
	}

	if len(req.UserBlocks) > 0 {
		blocks := make([]map[string]any, 0, len(req.UserBlocks)+len(req.Attachments))
		for _, b := range req.UserBlocks {
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != "" {
				block["cache_control"] = map[string]string{"type": b.CacheControl}
			}
			blocks = append(blocks, block)
		}
		// Add multimodal attachments (Anthropic)
		for _, att := range req.Attachments {
			data, err := os.ReadFile(att.Path)
			if err != nil {
				continue
			}
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": att.MimeType,
					"data":       base64.StdEncoding.EncodeToString(data),
				},
			})
		}
		body["messages"] = []map[string]any{{"role": "user", "content": blocks}}
	} else {
		if len(req.Attachments) > 0 {
			blocks := []map[string]any{{"type": "text", "text": req.User}}
			for _, att := range req.Attachments {
				data, err := os.ReadFile(att.Path)
				if err != nil {
					continue
				}
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": att.MimeType,
						"data":       base64.StdEncoding.EncodeToString(data),
					},
				})
			}
			body["messages"] = []map[string]any{{"role": "user", "content": blocks}}
		} else {
			body["messages"] = []map[string]any{{"role": "user", "content": req.User}}
		}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.MCPServers) > 0 {
		servers := make([]map[string]any, len(req.MCPServers))
		for i, s := range req.MCPServers {
			entry := map[string]any{"type": "url", "url": s.URL, "name": s.Name}
			if len(s.AllowedTools) > 0 {
				entry["allowed_tools"] = s.AllowedTools
			}
			servers[i] = entry
		}
		body["mcp_servers"] = servers
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", p.cfg.ResolveAPIKey(req.Secrets))
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	httpReq.Header.Set("content-type", "application/json")
	if tid := observability.GetTraceID(ctx); tid != "" {
		httpReq.Header.Set("x-trace-id", tid)
	}
	if sid := observability.GetSpanID(ctx); sid != "" {
		httpReq.Header.Set("x-span-id", sid)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxErrorBody)) // audit H1: cap error body
		if resp.StatusCode == 429 {
			return nil, &RateLimitError{Msg: string(b), RetryAfter: parseRetryAfter(resp.Header)}
		}
		if resp.StatusCode == 400 {
			return nil, &BadRequestError{Msg: string(b)}
		}
		return nil, &ProviderError{Msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(b))}
	}

	var fullText strings.Builder
	var toolCalls []ToolCall
	var stopReason string
	var currentToolCall *ToolCall
	var currentInput strings.Builder
	var cacheWrite, cacheRead int

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		lineStr := string(line)
		if !strings.HasPrefix(lineStr, "data: ") {
			continue
		}
		dataStr := strings.TrimSpace(lineStr[6:])
		if dataStr == "[DONE]" {
			break
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
			continue
		}

		switch event["type"] {
		case "content_block_start":
			if block, ok := event["content_block"].(map[string]any); ok {
				if block["type"] == "tool_use" {
					currentToolCall = &ToolCall{}
					if id, ok := block["id"].(string); ok {
						currentToolCall.CallID = id
					}
					if name, ok := block["name"].(string); ok {
						currentToolCall.Name = name
					}
					currentInput.Reset()
				}
			}
		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if delta["type"] == "text_delta" {
					if text, ok := delta["text"].(string); ok {
						fullText.WriteString(text)
						if err := onChunk(text); err != nil {
							return nil, err
						}
					}
				} else if delta["type"] == "input_json_delta" {
					if partial, ok := delta["partial_json"].(string); ok {
						currentInput.WriteString(partial)
					}
				}
			}
		case "content_block_stop":
			if currentToolCall != nil {
				var args map[string]any
				_ = json.Unmarshal([]byte(currentInput.String()), &args)
				if args == nil {
					args = make(map[string]any)
				}
				currentToolCall.Arguments = args
				toolCalls = append(toolCalls, *currentToolCall)
				currentToolCall = nil
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if sr, ok := delta["stop_reason"].(string); ok {
					stopReason = sr
				}
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				if cw, ok := usage["cache_creation_input_tokens"].(float64); ok {
					cacheWrite = int(cw)
				}
				if cr, ok := usage["cache_read_input_tokens"].(float64); ok {
					cacheRead = int(cr)
				}
			}
		}
	}

	return &ProviderResponse{
		Text:             fullText.String(),
		ToolCalls:        toolCalls,
		StopReason:       stopReason,
		CacheWriteTokens: cacheWrite,
		CacheReadTokens:  cacheRead,
	}, nil
}

func (p *AnthropicProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	baseURL := p.cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
	}

	// Handle block-based content for caching (Anthropic)
	if len(req.SystemBlocks) > 0 {
		blocks := make([]map[string]any, len(req.SystemBlocks))
		for i, b := range req.SystemBlocks {
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != "" {
				block["cache_control"] = map[string]string{"type": b.CacheControl}
			}
			blocks[i] = block
		}
		body["system"] = blocks
	} else {
		body["system"] = req.System
	}

	if len(req.UserBlocks) > 0 {
		blocks := make([]map[string]any, 0, len(req.UserBlocks)+len(req.Attachments))
		for _, b := range req.UserBlocks {
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != "" {
				block["cache_control"] = map[string]string{"type": b.CacheControl}
			}
			blocks = append(blocks, block)
		}
		// Add multimodal attachments (Anthropic)
		for _, att := range req.Attachments {
			data, err := os.ReadFile(att.Path)
			if err != nil {
				continue
			}
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": att.MimeType,
					"data":       base64.StdEncoding.EncodeToString(data),
				},
			})
		}
		body["messages"] = []map[string]any{{"role": "user", "content": blocks}}
	} else {
		if len(req.Attachments) > 0 {
			blocks := []map[string]any{{"type": "text", "text": req.User}}
			for _, att := range req.Attachments {
				data, err := os.ReadFile(att.Path)
				if err != nil {
					continue
				}
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": att.MimeType,
						"data":       base64.StdEncoding.EncodeToString(data),
					},
				})
			}
			body["messages"] = []map[string]any{{"role": "user", "content": blocks}}
		} else {
			body["messages"] = []map[string]any{{"role": "user", "content": req.User}}
		}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.MCPServers) > 0 {
		servers := make([]map[string]any, len(req.MCPServers))
		for i, s := range req.MCPServers {
			entry := map[string]any{"type": "url", "url": s.URL, "name": s.Name}
			if len(s.AllowedTools) > 0 {
				entry["allowed_tools"] = s.AllowedTools
			}
			servers[i] = entry
		}
		body["mcp_servers"] = servers
	}

	respBody, status, headers, err := p.post(ctx, baseURL+"/v1/messages", body, map[string]string{
		"x-api-key":         p.cfg.ResolveAPIKey(req.Secrets),
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "prompt-caching-2024-07-31",
		"content-type":      "application/json",
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
		Content []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Input struct {
				ToolUse struct {
					ID    string         `json:"id"`
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				} `json:"tool_use"`
			} `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &ProviderError{Msg: "failed to parse response: " + err.Error()}
	}

	var text strings.Builder
	var toolCalls []ToolCall
	for _, c := range resp.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		} else if c.Type == "tool_use" {
			toolCalls = append(toolCalls, ToolCall{
				CallID:    c.Input.ToolUse.ID,
				Name:      c.Input.ToolUse.Name,
				Arguments: c.Input.ToolUse.Input,
			})
		}
	}

	return &ProviderResponse{
		Text:             text.String(),
		ToolCalls:        toolCalls,
		StopReason:       resp.StopReason,
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
		CacheReadTokens:  resp.Usage.CacheReadInputTokens,
	}, nil
}

func (p *AnthropicProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by AnthropicProvider native API")
}
