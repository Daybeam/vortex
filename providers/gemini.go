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
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/pkg/safelimits"
)

type GeminiLoadBalancer struct {
	keys    []string
	current int
	mu      sync.Mutex
}

func NewGeminiLoadBalancer(keys []string) *GeminiLoadBalancer {
	return &GeminiLoadBalancer{keys: keys}
}

func (lb *GeminiLoadBalancer) NextKey() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if len(lb.keys) == 0 {
		return ""
	}
	key := lb.keys[lb.current]
	lb.current = (lb.current + 1) % len(lb.keys)
	return key
}

// ─── Gemini ────────────────────────────────────────────────────────────

type GeminiProvider struct {
	cfg    *config.ProviderConfig
	name   string
	client *http.Client
}

func (p *GeminiProvider) Name() string { return p.name }

// buildGeminiBody constructs the request body shared by Complete and StreamComplete.
func (p *GeminiProvider) buildGeminiBody(req CompleteRequest) map[string]any {
	userParts := []map[string]any{
		{"text": req.User},
	}

	for _, att := range req.Attachments {
		data, err := os.ReadFile(att.Path)
		if err != nil {
			continue
		}
		userParts = append(userParts, map[string]any{
			"inline_data": map[string]any{
				"mime_type": att.MimeType,
				"data":      base64.StdEncoding.EncodeToString(data),
			},
		})
	}

	body := map[string]any{
		"system_instruction": map[string]any{
			"parts": []map[string]string{{"text": req.System}},
		},
		"contents": []map[string]any{
			{"role": "user", "parts": userParts},
		},
		"generationConfig": map[string]any{"maxOutputTokens": req.MaxTokens},
	}
	if req.Temperature != nil {
		body["generationConfig"].(map[string]any)["temperature"] = *req.Temperature
	}

	// Native Tools Support
	if len(req.MCPServers) > 0 {
		var tools []map[string]any
		var functionDeclarations []map[string]any
		seen := make(map[string]bool) // FIX (2026-07-14): re-apply F8 dedup (regressed) - avoid Duplicate function declaration errors across MCPs
		for _, s := range req.MCPServers {
			for _, t := range s.Tools {
				if seen[t.Name] {
					continue
				}
				seen[t.Name] = true
				decl := map[string]any{
					"name":        t.Name,
					"description": t.Description,
				}
				if t.InputSchema != nil {
					decl["parameters"] = sanitizeGeminiSchema(t.InputSchema)
				}
				functionDeclarations = append(functionDeclarations, decl)
			}
		}
		if len(functionDeclarations) > 0 {
			tools = append(tools, map[string]any{
				"function_declarations": functionDeclarations,
			})
			body["tools"] = tools
			mode := "AUTO"
			const maxToolsForForcedCall = 200
			if req.ForceToolCall && len(functionDeclarations) <= maxToolsForForcedCall {
				mode = "ANY"
			}
			body["tool_config"] = map[string]any{
				"function_calling_config": map[string]any{
					"mode": mode,
				},
			}
		}
	}
	return body
}

func (p *GeminiProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	apiKey := p.cfg.ResolveAPIKey(req.Secrets)
	model := req.Model
	if after, ok := strings.CutPrefix(model, "models/"); ok {
		model = after
	}
	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse",
		model,
	)

	body := p.buildGeminiBody(req)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", apiKey)
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
	var toolCalls []ToolCall

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
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall *struct {
							Name string         `json:"name"`
							Args map[string]any `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Candidates) == 0 {
			continue
		}

		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.Text != "" {
				fullText.WriteString(part.Text)
				if onChunk != nil {
					if err := onChunk(part.Text); err != nil {
						return nil, err
					}
				}
			}
			if part.FunctionCall != nil {
				toolCalls = append(toolCalls, ToolCall{
					Name:      part.FunctionCall.Name,
					Arguments: part.FunctionCall.Args,
					CallID:    fmt.Sprintf("call_%d", time.Now().UnixNano()),
				})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if fullText.Len() > 0 || len(toolCalls) > 0 {
			return &ProviderResponse{Text: fullText.String(), ToolCalls: toolCalls}, nil
		}
		return nil, &ProviderError{Msg: "stream read error: " + err.Error()}
	}

	text := fullText.String()
	if text == "" && len(toolCalls) == 0 {
		return p.Complete(ctx, req)
	}

	return &ProviderResponse{Text: text, ToolCalls: toolCalls}, nil
}

func (p *GeminiProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	apiKey := p.cfg.ResolveAPIKey(req.Secrets)
	model := req.Model
	if after, ok := strings.CutPrefix(model, "models/"); ok {
		model = after
	}
	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent",
		model,
	)

	body := p.buildGeminiBody(req)

	respBody, status, headers, err := p.post(ctx, url, body, map[string]string{
		"Content-Type":   "application/json",
		"x-goog-api-key": apiKey,
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
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &ProviderError{Msg: "failed to parse response: " + err.Error()}
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, &ProviderError{Msg: "no content in response"}
	}

	var text strings.Builder
	var toolCalls []ToolCall
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			text.WriteString(part.Text)
		}
		if part.FunctionCall != nil {
			toolCalls = append(toolCalls, ToolCall{
				Name:      part.FunctionCall.Name,
				Arguments: part.FunctionCall.Args,
				CallID:    fmt.Sprintf("call_%d", time.Now().UnixNano()),
			})
		}
	}

	return &ProviderResponse{
		Text:             text.String(),
		ToolCalls:        toolCalls,
		PromptTokens:     resp.UsageMetadata.PromptTokenCount,
		CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
	}, nil
}

func (p *GeminiProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	apiKey := p.cfg.ResolveAPIKey(nil)
	model := p.cfg.EmbeddingModel
	if model == "" {
		model = "gemini-embedding-2"
	}
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/%s:embedContent",
		model,
	)

	body := map[string]any{
		"content": map[string]any{
			"parts": []map[string]any{
				{"text": text},
			},
		},
	}

	respBody, status, _, err := p.post(ctx, url, body, map[string]string{
		"Content-Type":   "application/json",
		"x-goog-api-key": apiKey,
	})
	if err != nil {
		return nil, err
	}
	if status == 429 {
		return nil, &RateLimitError{Msg: string(respBody), RetryAfter: parseRetryAfter(nil)}
	}
	if status >= 400 {
		return nil, &ProviderError{Msg: fmt.Sprintf("HTTP %d: %s", status, respBody)}
	}

	var resp struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &ProviderError{Msg: "failed to parse response: " + err.Error()}
	}
	if len(resp.Embedding.Values) == 0 {
		return nil, &ProviderError{Msg: "no embedding in response"}
	}
	return resp.Embedding.Values, nil
}

// maxGeminiRefDepth bounds $ref resolution recursion. JSON Schema allows
// circular $refs (used to express recursive types), which Gemini's schema
// format can't represent either way -- past this depth we give up and
// degrade to a generic object schema rather than recursing forever.
const maxGeminiRefDepth = 8

// sanitizeGeminiSchema converts an arbitrary JSON Schema (as commonly
// emitted by Pydantic/other JSON-Schema generators) into the restricted
// subset Gemini's function-calling API accepts.
//
// FIX (2026-08-15): previously only stripped a fixed set of unsupported
// keywords ($schema, additionalProperties, etc) but left $ref/$defs
// untouched. Gemini's generateContent API rejects the ENTIRE request with a
// 400 if any function_declarations[].parameters anywhere in the tree
// contains $ref or $defs -- discovered when melodie's tool schemas (Dict[str,
// SomeModel]-shaped fields, which Pydantic serializes as
// additionalProperties: {"$ref": "#/$defs/SomeModel"}) became reachable in a
// real completion request for the first time, breaking every Gemini call for
// any role bound to melodie. Rather than just deleting $ref (which would
// silently discard the actual type information a tool needs), this now
// resolves $ref pointers against the schema's own $defs/definitions map and
// inlines the resolved content, THEN strips the $defs/$ref/definitions
// machinery itself.
func sanitizeGeminiSchema(schema map[string]any) map[string]any {
	return resolveGeminiRefs(schema, extractGeminiDefs(schema), 0)
}

// extractGeminiDefs pulls a schema's top-level $defs (draft 2020-12) or
// definitions (older drafts) map, if present. $defs is conventionally
// declared once at the schema root even though $ref pointers to it can
// appear arbitrarily deep in properties/items/etc, so this is only called
// once per top-level tool schema and threaded through the recursive walk.
func extractGeminiDefs(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	if d, ok := schema["$defs"].(map[string]any); ok {
		return d
	}
	if d, ok := schema["definitions"].(map[string]any); ok {
		return d
	}
	return nil
}

func resolveGeminiRefs(schema map[string]any, defs map[string]any, depth int) map[string]any {
	if schema == nil {
		return nil
	}

	if ref, ok := schema["$ref"].(string); ok {
		if depth >= maxGeminiRefDepth {
			return map[string]any{"type": "object"}
		}
		name := strings.TrimPrefix(ref, "#/$defs/")
		name = strings.TrimPrefix(name, "#/definitions/")
		if target, ok := defs[name].(map[string]any); ok {
			return resolveGeminiRefs(target, defs, depth+1)
		}
		// Unresolvable $ref (name not found in defs, or a non-local $ref this
		// function doesn't support fetching) -- degrade to an untyped object
		// rather than leaving the $ref key in place, which Gemini would reject
		// outright just like an unresolved one.
		return map[string]any{"type": "object"}
	}

	forbidden := map[string]bool{
		"$schema":              true,
		"$defs":                true,
		"definitions":          true,
		"additionalProperties": true,
		"exclusiveMinimum":     true,
		"exclusiveMaximum":     true,
		"default":              true,
		"examples":             true,
		"pattern":              true,
		"minLength":            true,
		"maxLength":            true,
		"minItems":             true,
		"maxItems":             true,
		"uniqueItems":          true,
	}

	newSchema := make(map[string]any)
	for k, v := range schema {
		// Skip unsupported fields
		if forbidden[k] {
			continue
		}

		// Recursively handle nested objects and arrays
		switch val := v.(type) {
		case map[string]any:
			newSchema[k] = resolveGeminiRefs(val, defs, depth)
		case []any:
			newSchema[k] = resolveGeminiRefsArray(val, defs, depth)
		default:
			newSchema[k] = v
		}
	}
	return newSchema
}

func resolveGeminiRefsArray(arr []any, defs map[string]any, depth int) []any {
	newArr := make([]any, len(arr))
	for i, v := range arr {
		switch val := v.(type) {
		case map[string]any:
			newArr[i] = resolveGeminiRefs(val, defs, depth)
		case []any:
			newArr[i] = resolveGeminiRefsArray(val, defs, depth)
		default:
			newArr[i] = v
		}
	}
	return newArr
}
