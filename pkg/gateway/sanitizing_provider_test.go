package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
)

type mockProvider struct {
	lastReq      schemas.CompleteRequest
	lastEmbedText string
	resp         *schemas.ProviderResponse
	callCount    int
}

func (m *mockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	m.lastReq = req
	m.callCount++
	return m.resp, nil
}

func (m *mockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(text string) error) (*schemas.ProviderResponse, error) {
	m.lastReq = req
	m.callCount++
	return m.resp, nil
}

func (m *mockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	m.lastEmbedText = text
	return nil, nil
}

func (m *mockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}

func (m *mockProvider) Name() string {
	return "mock"
}

var _ interfaces.Provider = (*mockProvider)(nil)

func TestSanitizingProvider_MasksPromptAndUnmasksResponse(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-1")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{
		resp: &schemas.ProviderResponse{
			Text: "Result with [REDACTED_1] token",
			ToolCalls: []schemas.ToolCall{
				{Name: "call_api", Arguments: map[string]any{"key": "[REDACTED_1]"}},
			},
		},
	}

	p := NewSanitizingProvider(mock, sanitizer)
	ctx := ContextWithTaskID(context.Background(), "task-1")

	resp, err := p.Complete(ctx, schemas.CompleteRequest{
		System: "system prompt with " + secret,
		User:   "user prompt with " + secret,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(mock.lastReq.System, secret) {
		t.Error("system prompt was not masked — secret leaked to provider")
	}
	if strings.Contains(mock.lastReq.User, secret) {
		t.Error("user prompt was not masked — secret leaked to provider")
	}
	if !strings.Contains(mock.lastReq.User, "[REDACTED_1]") {
		t.Errorf("expected [REDACTED_1] in masked user prompt, got: %s", mock.lastReq.User)
	}

	if !strings.Contains(resp.Text, secret) {
		t.Errorf("response text was not unmasked — expected real secret, got: %s", resp.Text)
	}
	if resp.ToolCalls[0].Arguments["key"] != secret {
		t.Errorf("tool call arg was not unmasked — expected %s, got: %v", secret, resp.ToolCalls[0].Arguments["key"])
	}
}

func TestSanitizingProvider_PassThroughWithoutTaskID(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-1")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{
		resp: &schemas.ProviderResponse{Text: "response"},
	}

	p := NewSanitizingProvider(mock, sanitizer)
	ctx := context.Background()

	_, err := p.Complete(ctx, schemas.CompleteRequest{
		User: "prompt with " + secret,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(mock.lastReq.User, secret) {
		t.Error("without taskID in context, prompt should pass through unchanged")
	}
}

func TestSanitizingProvider_PassThroughWithNilSanitizer(t *testing.T) {
	mock := &mockProvider{
		resp: &schemas.ProviderResponse{Text: "response"},
	}

	p := NewSanitizingProvider(mock, nil)
	ctx := ContextWithTaskID(context.Background(), "task-1")

	_, err := p.Complete(ctx, schemas.CompleteRequest{
		User: "prompt with sk-abcdefghijklmnopqrstuvwxyz123456",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(mock.lastReq.User, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("with nil sanitizer, prompt should pass through unchanged")
	}
}

func TestSanitizingProvider_NestedMapUnmask(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-1")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{
		resp: &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "call", Arguments: map[string]any{
					"outer": map[string]any{
						"inner": "[REDACTED_1]",
					},
				}},
			},
		},
	}

	p := NewSanitizingProvider(mock, sanitizer)
	ctx := ContextWithTaskID(context.Background(), "task-1")

	resp, err := p.Complete(ctx, schemas.CompleteRequest{User: secret})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	outer := resp.ToolCalls[0].Arguments["outer"].(map[string]any)
	if outer["inner"] != secret {
		t.Errorf("nested map value was not unmasked — expected %s, got: %v", secret, outer["inner"])
	}
}

// C6.2 regression: masking must NOT mutate the caller's original data.
// Before the fix, maskBlocks/maskRequest modified slice backing arrays
// and maps in place, permanently replacing the caller's history with
// [REDACTED_N] tokens.
func TestSanitizingProvider_MaskingDoesNotMutateCallerData(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-c62")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	originalBlocks := []schemas.ContentBlock{{Text: "key: " + secret}}
	originalConstraints := map[string]any{"rule": "use " + secret + " for auth"}

	mock := &mockProvider{resp: &schemas.ProviderResponse{Text: "ok"}}
	p := NewSanitizingProvider(mock, sanitizer)

	req := schemas.CompleteRequest{
		User:        "use " + secret,
		UserBlocks:  originalBlocks,
		Constraints: originalConstraints,
	}
	ctx := ContextWithTaskID(context.Background(), "task-c62")

	_, _ = p.Complete(ctx, req)

	// Caller's original data must be untouched
	if originalBlocks[0].Text != "key: "+secret {
		t.Fatalf("caller's UserBlocks was mutated: got %q, want %q", originalBlocks[0].Text, "key: "+secret)
	}
	if originalConstraints["rule"] != "use "+secret+" for auth" {
		t.Fatalf("caller's Constraints was mutated: got %v", originalConstraints["rule"])
	}
}

// C6.3 regression: unmaskMap must handle []any (JSON arrays).
// Before the fix, tool call parameters with list items stayed masked.
func TestSanitizingProvider_UnmaskArrayItems(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-c63")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{resp: &schemas.ProviderResponse{
		Text: "ok",
		ToolCalls: []schemas.ToolCall{{
			Name: "transfer",
			Arguments: map[string]any{
				"accounts": []any{
					"act_123",
					sanitizer.Mask(secret, "task-c63"), // masked in transit
				},
				"nested": []any{
					map[string]any{"key": sanitizer.Mask(secret, "task-c63")},
				},
			},
		}},
	}}
	p := NewSanitizingProvider(mock, sanitizer)

	req := schemas.CompleteRequest{User: "test"}
	ctx := ContextWithTaskID(context.Background(), "task-c63")

	resp, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	accounts := resp.ToolCalls[0].Arguments["accounts"].([]any)
	if accounts[1] != secret {
		t.Errorf("array string item not unmasked: got %v, want %s", accounts[1], secret)
	}

	nested := resp.ToolCalls[0].Arguments["nested"].([]any)
	nestedMap := nested[0].(map[string]any)
	if nestedMap["key"] != secret {
		t.Errorf("array map item not unmasked: got %v, want %s", nestedMap["key"], secret)
	}
}

// C6.1 regression: Embed must mask text before passing to inner provider.
// Before the fix, Embed was pass-through, sending unmasked secrets to the
// embedding provider.
func TestSanitizingProvider_EmbedMasksText(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-c61")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{}
	p := NewSanitizingProvider(mock, sanitizer)

	ctx := ContextWithTaskID(context.Background(), "task-c61")
	_, _ = p.Embed(ctx, "use "+secret+" for auth")

	if strings.Contains(mock.lastEmbedText, secret) {
		t.Fatalf("Embed text was not masked — secret reached inner provider: %q", mock.lastEmbedText)
	}
	if !strings.Contains(mock.lastEmbedText, "[REDACTED_") {
		t.Fatalf("Embed text should contain [REDACTED_N] token, got: %q", mock.lastEmbedText)
	}
}

// C6.1: Embed without taskID should pass through (no masking).
func TestSanitizingProvider_EmbedPassThroughWithoutTaskID(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-c61b")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{}
	p := NewSanitizingProvider(mock, sanitizer)

	_, _ = p.Embed(context.Background(), "use "+secret)

	if mock.lastEmbedText != "use "+secret {
		t.Fatalf("Embed without taskID should pass through, got: %q", mock.lastEmbedText)
	}
}
