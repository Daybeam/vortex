package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// mockModelRecordingProvider records the model name passed to StreamComplete.
type mockModelRecordingProvider struct {
	lastModel string
	calls     int
}

func (m *mockModelRecordingProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *mockModelRecordingProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	m.lastModel = req.Model
	onChunk("OK.")
	return &schemas.ProviderResponse{Text: "OK."}, nil
}

func (m *mockModelRecordingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *mockModelRecordingProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *mockModelRecordingProvider) Name() string { return "mock-recording" }

// TestSendHandler_ModelOverrideRejectedNotConfigured verifies that the SendHandler
// rejects model_override for models not configured in the registry (audit NEW-1/NEW-2).
// Before the fix, unconfigured models silently fell back to the default harness.
// After the fix, they are rejected with HTTP 400.
func TestSendHandler_ModelOverrideRejectedNotConfigured(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	mockP := &mockModelRecordingProvider{}
	harness := &chat.ChatHarness{
		Provider:   mockP,
		Model:      "default-model",
		OutputBase: tmpDir,
		MaxTurns:   2,
	}

	chatStore := chat.NewChatSessionStore("", nil)
	handler := &ChatHandler{
		App:     app,
		Harness: harness,
		Store:   chatStore,
	}

	// Send a request with model_override set to an unconfigured model.
	body, _ := json.Marshal(map[string]string{
		"content":        "hello",
		"model_override": "nonexistent-model-xyz",
	})
	req := httptest.NewRequest("POST", "/api/chat/send", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.SendHandler(rr, req)

	if rr.Code != 400 {
		t.Fatalf("expected 400 for unconfigured model, got %d: %s", rr.Code, rr.Body.String())
	}
	handler.Shutdown()
}

// TestSendHandler_NoModelOverrideUsesDefault verifies that without model_override,
// the default harness model is used.
func TestSendHandler_NoModelOverrideUsesDefault(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	mockP := &mockModelRecordingProvider{}
	harness := &chat.ChatHarness{
		Provider:   mockP,
		Model:      "default-model",
		OutputBase: tmpDir,
		MaxTurns:   2,
	}

	chatStore := chat.NewChatSessionStore("", nil)
	handler := &ChatHandler{
		App:     app,
		Harness: harness,
		Store:   chatStore,
	}

	// Send a request without model_override.
	body, _ := json.Marshal(map[string]string{
		"content": "hello",
	})
	req := httptest.NewRequest("POST", "/api/chat/send", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.SendHandler(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// T-H15 fix: wait for the async goroutine to complete, then verify
	// the provider was called with the default model.
	handler.Shutdown()
	if mockP.calls == 0 {
		t.Error("expected provider to be called at least once, got 0 calls")
	}
	if mockP.lastModel != "default-model" {
		t.Errorf("expected default-model, got model=%q", mockP.lastModel)
	}
}
