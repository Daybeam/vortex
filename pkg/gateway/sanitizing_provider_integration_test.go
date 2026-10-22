package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestSanitizingProvider_MasksContentBlocks verifies that secrets in
// ContentBlock surfaces (SystemBlocks, ProtectedSystemBlocks, etc.) are
// masked before reaching the provider (P0 fix).
func TestSanitizingProvider_MasksContentBlocks(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-p0")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{
		resp: &schemas.ProviderResponse{Text: "ok"},
	}
	p := NewSanitizingProvider(mock, sanitizer)
	ctx := ContextWithTaskID(context.Background(), "task-p0")

	blocksWithSecret := []schemas.ContentBlock{
		{Text: "block with " + secret},
	}
	req := schemas.CompleteRequest{
		System:                "system with " + secret,
		User:                  "user with " + secret,
		SystemBlocks:          blocksWithSecret,
		UserBlocks:            []schemas.ContentBlock{{Text: "user block " + secret}},
		ProtectedSystemBlocks: []schemas.ContentBlock{{Text: "protected " + secret}},
		ProtectedUserBlocks:   []schemas.ContentBlock{{Text: "prot user " + secret}},
		VolatileSystemBlocks:  []schemas.ContentBlock{{Text: "volatile " + secret}},
		VolatileUserBlocks:    []schemas.ContentBlock{{Text: "vol user " + secret}},
		Constraints:           map[string]any{"grammar": "use key " + secret},
	}

	_, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []struct {
		name  string
		value string
	}{
		{"System", mock.lastReq.System},
		{"User", mock.lastReq.User},
		{"SystemBlocks[0]", mock.lastReq.SystemBlocks[0].Text},
		{"UserBlocks[0]", mock.lastReq.UserBlocks[0].Text},
		{"ProtectedSystemBlocks[0]", mock.lastReq.ProtectedSystemBlocks[0].Text},
		{"ProtectedUserBlocks[0]", mock.lastReq.ProtectedUserBlocks[0].Text},
		{"VolatileSystemBlocks[0]", mock.lastReq.VolatileSystemBlocks[0].Text},
		{"VolatileUserBlocks[0]", mock.lastReq.VolatileUserBlocks[0].Text},
		{"Constraints[grammar]", mock.lastReq.Constraints["grammar"].(string)},
	}
	for _, c := range checks {
		if strings.Contains(c.value, secret) {
			t.Errorf("%s: secret leaked to provider: %s", c.name, c.value)
		}
		if !strings.Contains(c.value, "[REDACTED_1]") {
			t.Errorf("%s: expected [REDACTED_1], got: %s", c.name, c.value)
		}
	}
}

// TestSanitizingProvider_VaultClearPreventsUnmask verifies that after
// Clear(taskID), stale tokens from the cleared vault cannot be restored
// unless a new Mask re-populates it (P2 fix).
func TestSanitizingProvider_VaultClearPreventsUnmask(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	taskID := "task-p2"

	masked := sanitizer.Mask(secret, taskID)
	if !strings.Contains(masked, "[REDACTED_1]") {
		t.Fatalf("expected [REDACTED_1], got: %s", masked)
	}
	unmasked := sanitizer.Unmask(masked, taskID)
	if unmasked != secret {
		t.Fatalf("before Clear: expected unmasked secret, got: %s", unmasked)
	}

	sanitizer.Clear(taskID)

	staleResult := sanitizer.Unmask(masked, taskID)
	if staleResult != masked {
		t.Errorf("after Clear: stale token should not be restored, got: %s", staleResult)
	}
	if strings.Contains(staleResult, secret) {
		t.Errorf("after Clear: secret leaked via stale vault, got: %s", staleResult)
	}
}

// TestSanitizingProvider_NilSanitizerPassThroughAllSurfaces verifies that
// a nil sanitizer is a complete pass-through across all surfaces (P1).
func TestSanitizingProvider_NilSanitizerPassThroughAllSurfaces(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	mock := &mockProvider{
		resp: &schemas.ProviderResponse{Text: "response"},
	}
	p := NewSanitizingProvider(mock, nil)
	ctx := ContextWithTaskID(context.Background(), "task-p1")

	req := schemas.CompleteRequest{
		System:                "system " + secret,
		User:                  "user " + secret,
		ProtectedSystemBlocks: []schemas.ContentBlock{{Text: "block " + secret}},
		Constraints:           map[string]any{"key": secret},
	}

	_, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(mock.lastReq.System, secret) {
		t.Error("nil sanitizer: System should pass through unchanged")
	}
	if !strings.Contains(mock.lastReq.ProtectedSystemBlocks[0].Text, secret) {
		t.Error("nil sanitizer: ProtectedSystemBlocks should pass through unchanged")
	}
	if mock.lastReq.Constraints["key"] != secret {
		t.Error("nil sanitizer: Constraints should pass through unchanged")
	}
}

// streamMockProvider is a mock that calls onChunk with specified chunks.
type streamMockProvider struct {
	lastReq schemas.CompleteRequest
	chunks  []string
	resp    *schemas.ProviderResponse
}

func (m *streamMockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.resp, nil
}

func (m *streamMockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(text string) error) (*schemas.ProviderResponse, error) {
	m.lastReq = req
	for _, c := range m.chunks {
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	return m.resp, nil
}

func (m *streamMockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func (m *streamMockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}

func (m *streamMockProvider) Name() string {
	return "stream-mock"
}

// TestStreamUnmasker_TokenWithinSingleChunk verifies that a token entirely
// within one chunk is unmasked correctly (P4 basic case).
func TestStreamUnmasker_TokenWithinSingleChunk(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-s1")
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"

	var received strings.Builder
	mock := &streamMockProvider{
		chunks: []string{"result [REDACTED_1] done"},
		resp:   &schemas.ProviderResponse{Text: "result [REDACTED_1] done"},
	}
	p := NewSanitizingProvider(mock, sanitizer)
	ctx := ContextWithTaskID(context.Background(), "task-s1")

	resp, err := p.StreamComplete(ctx, schemas.CompleteRequest{User: secret}, func(text string) error {
		received.WriteString(text)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	streamed := received.String()
	if strings.Contains(streamed, "[REDACTED_1]") {
		t.Errorf("streamed output should not contain masked token, got: %s", streamed)
	}
	if !strings.Contains(streamed, secret) {
		t.Errorf("streamed output should contain unmasked secret, got: %s", streamed)
	}
	if !strings.Contains(resp.Text, secret) {
		t.Errorf("response text should contain unmasked secret, got: %s", resp.Text)
	}
}

// TestStreamUnmasker_TokenSplitAcrossChunks verifies that a token split
// across chunk boundaries is correctly held back and unmasked (P4 key case).
func TestStreamUnmasker_TokenSplitAcrossChunks(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-s2")
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"

	var received strings.Builder
	mock := &streamMockProvider{
		chunks: []string{
			"result [REDA",
			"CTED_1] done",
		},
		resp: &schemas.ProviderResponse{Text: "result [REDACTED_1] done"},
	}
	p := NewSanitizingProvider(mock, sanitizer)
	ctx := ContextWithTaskID(context.Background(), "task-s2")

	resp, err := p.StreamComplete(ctx, schemas.CompleteRequest{User: secret}, func(text string) error {
		received.WriteString(text)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	streamed := received.String()
	if strings.Contains(streamed, "[REDACTED_1]") {
		t.Errorf("streamed output should not contain masked token, got: %s", streamed)
	}
	if !strings.Contains(streamed, secret) {
		t.Errorf("streamed output should contain unmasked secret, got: %s", streamed)
	}
	if !strings.Contains(resp.Text, secret) {
		t.Errorf("response text should contain unmasked secret, got: %s", resp.Text)
	}
}

// TestStreamUnmasker_TokenSplitAcrossThreeChunks verifies a token split
// across three chunks with the prefix broken up (P4 edge case).
func TestStreamUnmasker_TokenSplitAcrossThreeChunks(t *testing.T) {
	sanitizer := NewSanitizerProxy()
	defer sanitizer.Clear("task-s3")
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"

	var received strings.Builder
	mock := &streamMockProvider{
		chunks: []string{
			"pre [",
			"REDACT",
			"ED_1] post",
		},
		resp: &schemas.ProviderResponse{Text: "pre [REDACTED_1] post"},
	}
	p := NewSanitizingProvider(mock, sanitizer)
	ctx := ContextWithTaskID(context.Background(), "task-s3")

	resp, err := p.StreamComplete(ctx, schemas.CompleteRequest{User: secret}, func(text string) error {
		received.WriteString(text)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	streamed := received.String()
	if strings.Contains(streamed, "[REDACTED_1]") {
		t.Errorf("streamed output should not contain masked token, got: %s", streamed)
	}
	if !strings.Contains(streamed, secret) {
		t.Errorf("streamed output should contain unmasked secret, got: %s", streamed)
	}
	if !strings.Contains(resp.Text, secret) {
		t.Errorf("response text should contain unmasked secret, got: %s", resp.Text)
	}
}

// TestStreamUnmasker_NilSanitizerPassThrough verifies streaming pass-through
// when sanitizer is nil (P4 nil case).
func TestStreamUnmasker_NilSanitizerPassThrough(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"

	var received strings.Builder
	mock := &streamMockProvider{
		chunks: []string{"result " + secret + " done"},
		resp:   &schemas.ProviderResponse{Text: "result " + secret + " done"},
	}
	p := NewSanitizingProvider(mock, nil)
	ctx := ContextWithTaskID(context.Background(), "task-s4")

	resp, err := p.StreamComplete(ctx, schemas.CompleteRequest{User: secret}, func(text string) error {
		received.WriteString(text)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	streamed := received.String()
	if !strings.Contains(streamed, secret) {
		t.Errorf("nil sanitizer: streamed output should contain secret unchanged, got: %s", streamed)
	}
	if !strings.Contains(resp.Text, secret) {
		t.Errorf("nil sanitizer: response should contain secret unchanged, got: %s", resp.Text)
	}
}

// TestFindPartialToken verifies the partial token detection helper (P4 unit test).
func TestFindPartialToken(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"no token here", -1},
		{"result [REDACTED_1] done", -1},
		{"result [REDA", 7},
		{"result [REDACTED_", 7},
		{"result [REDACTED_1", 7},
		{"[", 0},
		{"[R", 0},
		{"[REDACT", 0},
		{"text [REDACTED_99", 5},
		{"text [REDACTED_99] more", -1},
		{"[REDACTED_1] [REDA", 13},
	}
	for _, c := range cases {
		got := findPartialToken(c.input)
		if got != c.want {
			t.Errorf("findPartialToken(%q) = %d, want %d", c.input, got, c.want)
		}
	}
}
