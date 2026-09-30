package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/daybeam/vortex/config"
)

// ─── System One Provider ──────────────────────────────────────────────────
//
// System One models (Jev, Laya, CUA-S1-FORMS, NanoJev) are non-autoregressive,
// parallel option scorers that produce structured probability distributions in
// a single forward pass. Unlike Chat LLMs that generate free text token-by-token,
// System One models score all candidate options simultaneously — ideal for
// high-frequency, low-variance, structured decision points (intent routing,
// tool selection, sieve judgment).
//
// This provider implements the /v1/systemone wire protocol — the de facto
// standard shared by Jev (TypeSafe), Laya (Convai), stuntd, laya-serve, and
// Cloudflare Workers AI (typesafe/jev). See docs/systemone-provider-design.md
// §0.4 for the protocol reconciliation and §0.4.4 for the wire format spec.

// SystemOneProvider connects to any /v1/systemone-compatible backend via HTTP.
type SystemOneProvider struct {
	cfg            *config.ProviderConfig
	client         *http.Client
	name           string
	scoreThreshold float64 // minimum confidence to include in ToolCalls (0 = include all)
	envelope       string  // "cloudflare" wraps body in {model, input}; "" = no envelope
}

// NewSystemOneProvider creates a System One provider backed by an HTTP endpoint.
func NewSystemOneProvider(cfg *config.ProviderConfig) *SystemOneProvider {
	timeout := 30 * time.Second // hard upper limit; System One should be ms-level
	if ms, ok := cfg.Extra["timeout_ms"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	threshold := 0.0
	if t, ok := cfg.Extra["score_threshold"].(float64); ok {
		threshold = t
	}

	envelope := ""
	if e, ok := cfg.Extra["envelope"].(string); ok {
		envelope = e
	}

	return &SystemOneProvider{
		cfg:            cfg,
		name:           "systemone:" + cfg.Model,
		client:         &http.Client{Timeout: timeout},
		scoreThreshold: threshold,
		envelope:       envelope,
	}
}

func (p *SystemOneProvider) Name() string { return p.name }

// ── /v1/systemone wire types (§0.4.1) ─────────────────────────────────────

// systemOneRequest is the JSON body POSTed to the /v1/systemone endpoint.
// Questions is map[string]any because the caller builds it from Constraints,
// and question criteria vary by type (map for choice, array for score).
type systemOneRequest struct {
	State     string         `json:"state"`
	Model     string         `json:"model,omitempty"`
	Questions map[string]any `json:"questions"`
}

// cloudflareRequest wraps the standard request in Cloudflare Workers AI envelope.
// Cloudflare is the only backend that requires this wrapping (§0.4.2).
type cloudflareRequest struct {
	Model string           `json:"model"`
	Input systemOneRequest `json:"input"`
}

// systemOneResponse is the JSON body returned by the /v1/systemone endpoint.
type systemOneResponse struct {
	Model   string                     `json:"model,omitempty"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   *systemOneUsage            `json:"usage,omitempty"`
}

// systemOneAnswer is a single question's answer. Fields are populated based on Type:
//   - "choice": Choice + Confidence + Probabilities
//   - "score":  Score + Confidence + Legend + Probabilities
//   - "noul":   Noul + Confidence
type systemOneAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

type systemOneUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// ── Provider interface ────────────────────────────────────────────────────

func (p *SystemOneProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	if p.cfg.BaseURL == "" {
		return nil, fmt.Errorf("systemone provider %q: base_url is required", p.name)
	}

	// Extract questions from Constraints (§0.1: CompleteRequest has no Extra
	// field; System One uses Constraints for decision context).
	questions, ok := req.Constraints["questions"].(map[string]any)
	if !ok || len(questions) == 0 {
		return nil, fmt.Errorf("systemone provider %q: Constraints[\"questions\"] must be a non-empty map[string]any", p.name)
	}

	// State: prefer User string, fall back to System string.
	state := req.User
	if state == "" {
		state = req.System
	}

	body := systemOneRequest{
		State:     state,
		Model:     p.cfg.Model,
		Questions: questions,
	}

	// Cloudflare envelope wrapping (§0.4.2: only Cloudflare needs this).
	var payload []byte
	var err error
	if p.envelope == "cloudflare" {
		envelope := cloudflareRequest{Model: p.cfg.Model, Input: body}
		payload, err = json.Marshal(envelope)
	} else {
		payload, err = json.Marshal(body)
	}
	if err != nil {
		return nil, fmt.Errorf("systemone provider %q: marshal request: %w", p.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("systemone provider %q: build request: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Bearer auth (§0.4.4: from cfg.APIKey; empty = no header for local self-hosted).
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("systemone provider %q: http call: %w", p.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("systemone provider %q: backend returned HTTP %d: %s", p.name, resp.StatusCode, string(raw))
	}

	var result systemOneResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("systemone provider %q: decode response: %w", p.name, err)
	}

	// Map answers to ToolCalls. Each ToolCall.Name = question id,
	// ToolCall.Arguments carries the typed answer fields for downstream routing.
	// score_threshold filters low-confidence answers (abstention).
	toolCalls := make([]ToolCall, 0, len(result.Answers))
	for qID, ans := range result.Answers {
		if p.scoreThreshold > 0 && ans.Confidence < p.scoreThreshold {
			continue
		}
		args := map[string]any{
			"type":       ans.Type,
			"confidence": ans.Confidence,
		}
		switch ans.Type {
		case "choice":
			args["choice"] = ans.Choice
			if len(ans.Probabilities) > 0 {
				args["probabilities"] = ans.Probabilities
			}
		case "score":
			args["score"] = ans.Score
			if len(ans.Legend) > 0 {
				args["legend"] = ans.Legend
			}
			if len(ans.Probabilities) > 0 {
				args["probabilities"] = ans.Probabilities
			}
		case "noul":
			args["noul"] = ans.Noul
		}
		toolCalls = append(toolCalls, ToolCall{
			Name:      qID,
			Arguments: args,
		})
	}

	return &ProviderResponse{
		Text:       "", // System One produces no free text
		ToolCalls:  toolCalls,
		StopReason: "scored",
	}, nil
}

// StreamComplete delegates to Complete — System One is non-autoregressive
// and has no streaming semantics. The onChunk callback is never invoked.
func (p *SystemOneProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(text string) error) (*ProviderResponse, error) {
	return p.Complete(ctx, req)
}

// Embed is not supported by System One models.
func (p *SystemOneProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("systemone provider %q: embed not supported", p.name)
}

// CountTokens estimates tokens using the shared heuristic (len/4).
func (p *SystemOneProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return estimateTokens(text), nil
}
