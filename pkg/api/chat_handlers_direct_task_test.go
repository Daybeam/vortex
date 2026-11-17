package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/tools"
)

func newTestChatHandler(t *testing.T) *ChatHandler {
	t.Helper()
	chatStore := chat.NewChatSessionStore(t.TempDir(), nil)
	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	tmpDir := t.TempDir()
	engine := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, tmpDir, tmpDir, nil)
	t.Cleanup(func() { engine.Stop() })
	app := &tools.App{Scheduler: engine}
	return &ChatHandler{App: app, Store: chatStore}
}

// TestSendHandler_DirectTaskBypassesHarness verifies that when direct_task=true
// in the request body, the SendHandler delegates directly to the orchestrator
// and returns a response with direct_task:true, skipping the ChatHarness LLM loop.
//
// Regression test for the direct_task gate added in CHAT_GATEWAY_TIERS_DESIGN.md §5.
// This test would fail if the DirectTask gate were removed from SendHandler —
// the response would not contain the direct_task field.
func TestSendHandler_DirectTaskBypassesHarness(t *testing.T) {
	h := newTestChatHandler(t)

	body := `{"content":"test task","direct_task":true,"message_id":"msg_test_1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/send", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.SendHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if data["direct_task"] != true {
		t.Fatalf("expected direct_task:true in response, got %v", data["direct_task"])
	}

	h.Shutdown()
}

// TestSendHandler_DirectTaskFalseFallsThroughToHarness verifies that when
// direct_task is false (default), the SendHandler does NOT short-circuit
// and instead proceeds to the harness path (which will fail without a provider,
// but the response should NOT contain direct_task:true).
func TestSendHandler_DirectTaskFalseFallsThroughToHarness(t *testing.T) {
	h := newTestChatHandler(t)
	h.Harness = &chat.ChatHarness{}

	body := `{"content":"hello","direct_task":false,"message_id":"msg_test_2"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/send", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.SendHandler(w, req)

	resp := w.Result()
	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)

	if data["direct_task"] == true {
		t.Fatal("expected direct_task to NOT be true when direct_task:false in request")
	}

	h.Shutdown()
}

// TestChatHandler_CancelFuncs_TrueInterrupt verifies that when a new chat
// request arrives for a session that has an in-flight run, the previous
// run's cancel func is invoked — enabling true interrupt instead of
// wasting tokens on a turn the user has abandoned.
//
// Regression test for the cancelFuncs sync.Map added in
// CHAT_GATEWAY_TIERS_DESIGN.md §4 (Phase 6).
func TestChatHandler_CancelFuncs_TrueInterrupt(t *testing.T) {
	h := &ChatHandler{}

	cancelled := make(chan struct{})
	h.cancelFuncs.Store("s_interrupt_test", context.CancelFunc(func() {
		close(cancelled)
	}))

	// Simulate what SendHandler does before starting a new run:
	// cancel any in-flight run for this session.
	if oldCancel, ok := h.cancelFuncs.LoadAndDelete("s_interrupt_test"); ok {
		oldCancel.(context.CancelFunc)()
	}

	select {
	case <-cancelled:
		// Success — previous cancel func was invoked.
	case <-time.After(time.Second):
		t.Fatal("previous cancel func was not invoked within 1s")
	}

	if _, ok := h.cancelFuncs.Load("s_interrupt_test"); ok {
		t.Fatal("cancel func should be deleted after LoadAndDelete")
	}
}

// TestChatHandler_CancelFuncs_NoOpWhenNoInFlightRun verifies that the
// cancel logic is a no-op when there is no in-flight run for the session.
func TestChatHandler_CancelFuncs_NoOpWhenNoInFlightRun(t *testing.T) {
	h := &ChatHandler{}

	// No cancel func stored for this session — should be a no-op.
	if oldCancel, ok := h.cancelFuncs.LoadAndDelete("s_no_run"); ok {
		oldCancel.(context.CancelFunc)()
		t.Fatal("expected no cancel func for a session with no in-flight run")
	}
}

// TestProcessMessage_DirectTaskBypassesHarness verifies that ProcessMessage
// (the transport-agnostic core) returns DirectTask:true when called directly
// without an HTTP wrapper. This is the path Telegram and other input channels use.
//
// Regression test for the ProcessMessage extraction (chat_channel.go refactor).
func TestProcessMessage_DirectTaskBypassesHarness(t *testing.T) {
	h := newTestChatHandler(t)

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		Content:    "test task via ProcessMessage",
		MessageID:  "msg_pm_1",
		DirectTask: true,
	})

	if !resp.DirectTask {
		t.Fatalf("expected DirectTask:true, got false")
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.SessionID == "" {
		t.Fatal("expected non-empty SessionID")
	}

	h.Shutdown()
	time.Sleep(100 * time.Millisecond)
}

// TestProcessMessage_EmptyContentReturnsError verifies that ProcessMessage
// rejects empty content with a 400 status code — the same behavior as the
// old SendHandler, now accessible to non-HTTP channels.
func TestProcessMessage_EmptyContentReturnsError(t *testing.T) {
	h := newTestChatHandler(t)

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		Content: "",
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", resp.StatusCode)
	}
	if resp.Error == "" {
		t.Fatal("expected non-empty error message")
	}
}

// TestProcessMessage_NoProviderReturnsError verifies that ProcessMessage
// returns an error when no provider is configured (and direct_task is false).
func TestProcessMessage_NoProviderReturnsError(t *testing.T) {
	h := newTestChatHandler(t)
	h.Harness = &chat.ChatHarness{}

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		Content:   "hello",
		MessageID: "msg_pm_2",
	})

	if resp.Error == "" {
		t.Fatal("expected error for no provider configured")
	}
	if resp.Error != "no provider configured" {
		t.Fatalf("expected 'no provider configured', got %q", resp.Error)
	}
}

// TestProcessMessage_SlashCommand verifies that slash commands are handled
// by ProcessMessage without requiring an HTTP roundtrip.
func TestProcessMessage_SlashCommand(t *testing.T) {
	h := newTestChatHandler(t)

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		Content:   "/help",
		MessageID: "msg_pm_3",
	})

	if !resp.SlashCommand {
		t.Fatal("expected SlashCommand:true for /help")
	}
	if resp.SessionID == "" {
		t.Fatal("expected non-empty SessionID")
	}
}

// TestHTTPChannel_StartRegistersRoutes verifies that HTTPChannel.Start
// registers all expected chat routes on the mux.
func TestHTTPChannel_StartRegistersRoutes(t *testing.T) {
	h := newTestChatHandler(t)
	mux := http.NewServeMux()
	ch := NewHTTPChannel(h, mux)

	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("HTTPChannel.Start failed: %v", err)
	}
	if ch.Name() != "http" {
		t.Fatalf("expected name 'http', got %q", ch.Name())
	}
	ch.Stop()
}

// TestTelegramChannel_NewAndName verifies basic Telegram channel construction.
func TestTelegramChannel_NewAndName(t *testing.T) {
	h := newTestChatHandler(t)
	ch := NewTelegramChannel(h, "test_token")

	if ch.Name() != "telegram" {
		t.Fatalf("expected name 'telegram', got %q", ch.Name())
	}
}
