package gateway

import (
	"context"
	"strings"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
)

type taskIDKey struct{}

func ContextWithTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, taskID)
}

func taskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(taskIDKey{}).(string); ok {
		return v
	}
	return ""
}

// SanitizingProvider wraps a Provider with bidirectional PII tokenization.
// Outbound prompts get secrets masked; inbound responses get tokens restored.
// When taskID is absent from context or sanitizer is nil, it is a pass-through.
type SanitizingProvider struct {
	inner     interfaces.Provider
	sanitizer *SanitizerProxy
}

func NewSanitizingProvider(inner interfaces.Provider, sanitizer *SanitizerProxy) *SanitizingProvider {
	return &SanitizingProvider{inner: inner, sanitizer: sanitizer}
}

func (p *SanitizingProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	taskID := taskIDFromContext(ctx)
	if taskID != "" && p.sanitizer != nil {
		p.maskRequest(&req, taskID)
	}
	resp, err := p.inner.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if taskID != "" && p.sanitizer != nil {
		p.unmaskResponse(resp, taskID)
	}
	return resp, nil
}

func (p *SanitizingProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(text string) error) (*schemas.ProviderResponse, error) {
	taskID := taskIDFromContext(ctx)
	if taskID != "" && p.sanitizer != nil {
		p.maskRequest(&req, taskID)
	}

	wrappedOnChunk := onChunk
	var unmasker *streamUnmasker
	if taskID != "" && p.sanitizer != nil {
		unmasker = &streamUnmasker{
			sanitizer: p.sanitizer,
			taskID:    taskID,
			onChunk:   onChunk,
		}
		wrappedOnChunk = unmasker.chunk
	}

	resp, err := p.inner.StreamComplete(ctx, req, wrappedOnChunk)
	if err != nil {
		return nil, err
	}
	if unmasker != nil {
		if flushErr := unmasker.flush(); flushErr != nil {
			return nil, flushErr
		}
	}
	if taskID != "" && p.sanitizer != nil {
		p.unmaskResponse(resp, taskID)
	}
	return resp, nil
}

func (p *SanitizingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	// C6.1: mask text before embedding. Previously pass-through, which
	// sent unmasked secrets to the embedding provider.
	taskID := taskIDFromContext(ctx)
	if taskID != "" && p.sanitizer != nil {
		text = p.sanitizer.Mask(text, taskID)
	}
	return p.inner.Embed(ctx, text)
}

func (p *SanitizingProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return p.inner.CountTokens(ctx, text)
}

func (p *SanitizingProvider) Name() string {
	return p.inner.Name()
}

func (p *SanitizingProvider) unmaskResponse(resp *schemas.ProviderResponse, taskID string) {
	resp.Text = p.sanitizer.Unmask(resp.Text, taskID)
	for i := range resp.ToolCalls {
		unmaskMap(p.sanitizer, resp.ToolCalls[i].Arguments, taskID)
	}
}

// maskRequest applies PII tokenization to all prompt surfaces (P0 fix).
// Masks System/User strings + all 6 ContentBlock slices + Constraints.
// Ensures no secret reaches the provider via any surface.
// C6.2: deep-copies slices and maps before masking to avoid mutating
// the caller's data (spawner's userBlocks accumulate across turns).
func (p *SanitizingProvider) maskRequest(req *schemas.CompleteRequest, taskID string) {
	req.System = p.sanitizer.Mask(req.System, taskID)
	req.User = p.sanitizer.Mask(req.User, taskID)
	req.SystemBlocks = p.maskBlocks(req.SystemBlocks, taskID)
	req.UserBlocks = p.maskBlocks(req.UserBlocks, taskID)
	req.ProtectedSystemBlocks = p.maskBlocks(req.ProtectedSystemBlocks, taskID)
	req.ProtectedUserBlocks = p.maskBlocks(req.ProtectedUserBlocks, taskID)
	req.VolatileSystemBlocks = p.maskBlocks(req.VolatileSystemBlocks, taskID)
	req.VolatileUserBlocks = p.maskBlocks(req.VolatileUserBlocks, taskID)
	if req.Constraints != nil {
		cp := make(map[string]any, len(req.Constraints))
		for k, v := range req.Constraints {
			if s, ok := v.(string); ok {
				cp[k] = p.sanitizer.Mask(s, taskID)
			} else {
				cp[k] = v
			}
		}
		req.Constraints = cp
	}
}

// maskBlocks returns a new slice with masked text. C6.2: deep-copies the
// slice to avoid mutating the caller's backing array.
func (p *SanitizingProvider) maskBlocks(blocks []schemas.ContentBlock, taskID string) []schemas.ContentBlock {
	if len(blocks) == 0 {
		return blocks
	}
	cp := make([]schemas.ContentBlock, len(blocks))
	copy(cp, blocks)
	for i := range cp {
		cp[i].Text = p.sanitizer.Mask(cp[i].Text, taskID)
	}
	return cp
}

func unmaskMap(s *SanitizerProxy, m map[string]any, taskID string) {
	for k, v := range m {
		switch val := v.(type) {
		case string:
			m[k] = s.Unmask(val, taskID)
		case map[string]any:
			unmaskMap(s, val, taskID)
		case []any:
			// C6.3: handle JSON arrays (tool call parameters with list items)
			for i, item := range val {
				switch it := item.(type) {
				case string:
					val[i] = s.Unmask(it, taskID)
				case map[string]any:
					unmaskMap(s, it, taskID)
				}
			}
		}
	}
}

const tokenPrefix = "[REDACTED_"

// findPartialToken returns the start index of a potential incomplete [REDACTED_N]
// token at the end of text, or -1 if none. Used by streamUnmasker to hold back
// partial tokens that may span chunk boundaries (P4).
func findPartialToken(text string) int {
	maxLen := len(tokenPrefix) + 7
	start := len(text) - maxLen
	if start < 0 {
		start = 0
	}
	for i := len(text) - 1; i >= start; i-- {
		if text[i] != '[' {
			continue
		}
		rest := text[i:]
		if len(rest) < len(tokenPrefix) && strings.HasPrefix(tokenPrefix, rest) {
			return i
		}
		if strings.HasPrefix(rest, tokenPrefix) {
			afterPrefix := rest[len(tokenPrefix):]
			if strings.HasSuffix(afterPrefix, "]") {
				continue
			}
			if afterPrefix == "" || isAllDigits(afterPrefix) {
				return i
			}
		}
	}
	return -1
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// streamUnmasker wraps an onChunk callback to unmask streaming text,
// holding back partial tokens at chunk boundaries (P4).
type streamUnmasker struct {
	sanitizer *SanitizerProxy
	taskID    string
	pending   string
	onChunk   func(text string) error
}

func (s *streamUnmasker) chunk(text string) error {
	combined := s.pending + text
	s.pending = ""

	cutAt := findPartialToken(combined)
	if cutAt < 0 {
		unmasked := s.sanitizer.Unmask(combined, s.taskID)
		if unmasked != "" {
			return s.onChunk(unmasked)
		}
		return nil
	}

	safe := combined[:cutAt]
	s.pending = combined[cutAt:]
	unmasked := s.sanitizer.Unmask(safe, s.taskID)
	if unmasked != "" {
		return s.onChunk(unmasked)
	}
	return nil
}

func (s *streamUnmasker) flush() error {
	if s.pending == "" {
		return nil
	}
	unmasked := s.sanitizer.Unmask(s.pending, s.taskID)
	s.pending = ""
	if unmasked != "" {
		return s.onChunk(unmasked)
	}
	return nil
}
