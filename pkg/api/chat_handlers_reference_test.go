package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// captureProvider captures the User field from the first Complete/StreamComplete
// call and returns a simple text response with no tool calls (terminates loop).
type captureProvider struct {
	mu        sync.Mutex
	captured  string
	callCount int
}

func (p *captureProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *captureProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.mu.Lock()
	p.callCount++
	if p.callCount == 1 {
		p.captured = req.User
	}
	p.mu.Unlock()
	onChunk("OK.")
	return &schemas.ProviderResponse{Text: "OK."}, nil
}

func (p *captureProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func (p *captureProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}

func (p *captureProvider) Name() string { return "capture" }

func (p *captureProvider) getCaptured() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.captured
}

// TestProcessMessage_ExactReferenceInjection verifies that when ReplyToContent
// is set on the ChatRequest, the referenced message content is injected into
// the history passed to the provider.
//
// Regression test for the exact reference feature (reply-to) in
// plan/LONG_CONVERSATION_REFERENCE.md §4.1.
func TestProcessMessage_ExactReferenceInjection(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}
	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
	}

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID:      "test_ref_session",
		MessageID:      "msg_ref_1",
		Content:        "What about that thing?",
		ReplyTo:        "old_msg_42",
		ReplyToContent: "The quick brown fox jumps over the lazy dog",
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.SessionID != "test_ref_session" {
		t.Fatalf("expected session 'test_ref_session', got %s", resp.SessionID)
	}

	time.Sleep(200 * time.Millisecond)
	h.Shutdown()

	captured := prov.getCaptured()
	if captured == "" {
		t.Fatal("provider was not called")
	}
	if !strings.Contains(captured, "Referenced message") {
		t.Fatalf("expected 'Referenced message' in captured input, got: %s", captured)
	}
	if !strings.Contains(captured, "quick brown fox") {
		t.Fatalf("expected referenced content in captured input, got: %s", captured)
	}
}

// TestProcessMessage_ReferenceInjectionOrder verifies the quotation is
// injected BEFORE the new user message, not after it.
//
// Regression (2026-10-24): the injection used to append after the
// already-persisted new message, which (a) violated the design spec
// ("inject as a quotation before the new message",
// CHAT_BRANCHING_AND_QUOTE_DESIGN.md §4) and (b) made ChatHarness.Run's
// `query` — the last user message, used to build the system prompt —
// resolve to the QUOTED text instead of the user's actual reply.
func TestProcessMessage_ReferenceInjectionOrder(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}
	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
	}

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID:      "test_ref_order_session",
		MessageID:      "msg_order_1",
		Content:        "What about that thing?",
		ReplyTo:        "old_msg_42",
		ReplyToContent: "The quick brown fox jumps over the lazy dog",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	time.Sleep(200 * time.Millisecond)
	h.Shutdown()

	captured := prov.getCaptured()
	if captured == "" {
		t.Fatal("provider was not called")
	}
	quoteIdx := strings.Index(captured, "[Referenced message]")
	replyIdx := strings.Index(captured, "What about that thing?")
	if quoteIdx < 0 {
		t.Fatalf("expected referenced message in captured input, got: %s", captured)
	}
	if replyIdx < 0 {
		t.Fatalf("expected the user's reply in captured input, got: %s", captured)
	}
	if quoteIdx > replyIdx {
		t.Fatalf("quotation must precede the new user message (quote at %d, reply at %d): %s", quoteIdx, replyIdx, captured)
	}
}

// TestProcessMessage_ExactReferenceLookupFromSession verifies that when
// ReplyTo is set but ReplyToContent is empty, ProcessMessage looks up the
// message content from the session tree by message ID.
func TestProcessMessage_ExactReferenceLookupFromSession(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}
	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
	}

	sessionID := "test_lookup_session"
	session := h.Store.GetOrCreate(sessionID)

	session.AppendUserMessage("ext_msg_1", "Remember: the answer is 42", "", "")
	_ = h.Store.Persist(session)

	internalID := ""
	for id := range session.Messages {
		internalID = id
		break
	}
	if internalID == "" {
		t.Fatal("expected at least one message in session")
	}

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: sessionID,
		MessageID: "msg_ref_2",
		Content:   "What was the answer?",
		ReplyTo:   internalID,
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	time.Sleep(200 * time.Millisecond)
	h.Shutdown()

	captured := prov.getCaptured()
	if captured == "" {
		t.Fatal("provider was not called")
	}
	if !strings.Contains(captured, "the answer is 42") {
		t.Fatalf("expected looked-up content in captured input, got: %s", captured)
	}
}

// TestProcessMessage_NoReferenceNoInjection verifies that when neither ReplyTo
// nor ReplyToContent is set, no referenced message block is injected.
func TestProcessMessage_NoReferenceNoInjection(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}
	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
	}

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: "test_no_ref",
		MessageID: "msg_no_ref_1",
		Content:   "Just a normal message",
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	time.Sleep(200 * time.Millisecond)
	h.Shutdown()

	captured := prov.getCaptured()
	if strings.Contains(captured, "Referenced message") {
		t.Fatalf("expected no referenced message block, got: %s", captured)
	}
}

// TestProcessMessage_SemanticRecallIndexing verifies that after a chat turn
// completes, the turn is indexed in ContextArchive with the correct taskID
// scope ("chat_" + sessionID), making it retrievable by buildSystem's
// SearchForest call.
func TestProcessMessage_SemanticRecallIndexing(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}

	archive := core.NewContextArchive(t.TempDir())

	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
		Archive:  archive,
		Embed:    &noopEmbeddingClient{},
	}

	sessionID := "test_semantic_session"
	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: sessionID,
		MessageID: "msg_semantic_1",
		Content:   "Tell me about Go concurrency",
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	time.Sleep(300 * time.Millisecond)
	h.Shutdown()

	taskID := "chat_" + sessionID
	// projectScope sits between taskScope and taskID since ef99779d6
	// (Project Space); "default" is what ProjectOf returns for a task with no
	// project set, which is every task this test creates.
	results := archive.Search("Go concurrency", &noopEmbeddingClient{}, 5, taskID, "default", taskID)
	if len(results) == 0 {
		t.Fatal("expected at least one search result for this chat session")
	}
	found := false
	for _, item := range results {
		if strings.Contains(item.Intent, "Go concurrency") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected search result with intent containing 'Go concurrency', got %d results", len(results))
	}
}

// noopEmbeddingClient is a minimal EmbeddingClient for tests that don't
// need real embeddings.
type noopEmbeddingClient struct{}

func (n *noopEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// TestProcessMessage_NoArchiveNoIndexing verifies that when Archive is nil,
// the indexing path is skipped without error (graceful degradation).
func TestProcessMessage_NoArchiveNoIndexing(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}

	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
	}

	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: "test_no_archive",
		MessageID: "msg_no_archive_1",
		Content:   "test without archive",
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	time.Sleep(200 * time.Millisecond)
	h.Shutdown()

	if prov.getCaptured() == "" {
		t.Fatal("provider should still be called even without archive")
	}
}

// TestHTTPChannel_ReplyToFromHeader verifies that the HTTP channel can pass
// reply_to and reply_to_content through the request body.
func TestHTTPChannel_ReplyToFromHeader(t *testing.T) {
	h := newTestChatHandler(t)
	prov := &captureProvider{}
	h.Harness = &chat.ChatHarness{
		Provider: prov,
		Model:    "capture",
		MaxTurns: 1,
	}

	body := `{"content":"follow up","message_id":"msg_http_ref","reply_to":"old_msg","reply_to_content":"context from before"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/send", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.SendHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	time.Sleep(200 * time.Millisecond)
	h.Shutdown()

	captured := prov.getCaptured()
	if captured == "" {
		t.Fatal("provider was not called")
	}
	if !strings.Contains(captured, "context from before") {
		t.Fatalf("expected reply_to_content in captured input, got: %s", captured)
	}
}
