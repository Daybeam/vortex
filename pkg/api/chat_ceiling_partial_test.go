package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// Q5-3 on the API side (see pkg/chat/chat_harness_ceiling_test.go for the
// harness half). The turn ceiling used to be discarded like any provider
// failure: ProcessMessage gated everything on err == nil, so a run that
// produced four turns of prose and then ran out of budget left the user with
// nothing in the transcript.
//
// The fix must keep both guarantees:
//	① the marked partial lands in the transcript;
//	② it does NOT land in the ContextArchive retrieval index.
//
// Fails before the fix: assertion ① (nothing was appended at all).

// ceilingAPIProvider streams prose and issues one distinct write_file per turn,
// so the loop reaches the ceiling with a real partial accumulated.
type ceilingAPIProvider struct {
	calls int
}

func (p *ceilingAPIProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *ceilingAPIProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	text := fmt.Sprintf("working on step %d. ", p.calls)
	onChunk(text)
	return &schemas.ProviderResponse{
		Text: text,
		ToolCalls: []schemas.ToolCall{
			{Name: "write_file", Arguments: map[string]any{
				"path":    fmt.Sprintf("part%d.txt", p.calls),
				"content": fmt.Sprintf("chunk %d", p.calls),
			}},
		},
	}, nil
}

func (p *ceilingAPIProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (p *ceilingAPIProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *ceilingAPIProvider) Name() string                                              { return "ceilingapi" }

// waitForAssistant polls the session until an assistant message appears or the
// deadline passes — ProcessMessage runs the turn in a goroutine.
func waitForAssistant(t *testing.T, h *ChatHandler, sessionID string, timeout time.Duration) []chat.ChatMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := h.Store.Get(sessionID); s != nil {
			msgs := s.SnapshotMessages()
			for _, m := range msgs {
				if m.Role == "assistant" {
					return msgs
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no assistant message appeared in the session before the deadline")
	return nil
}

func TestProcessMessage_CeilingPartialKeptButNotIndexed(t *testing.T) {
	h := newTestChatHandler(t)
	sessionID := "test_ceiling_partial"
	taskID := "chat_" + sessionID

	outBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outBase, taskID), 0755); err != nil {
		t.Fatal(err)
	}
	archive := core.NewContextArchive(filepath.Join(t.TempDir(), "chat_ctx.jsonl"))

	h.Harness = &chat.ChatHarness{
		Provider:   &ceilingAPIProvider{},
		Model:      "ceilingapi",
		MaxTurns:   3,
		OutputBase: outBase,
		Archive:    archive,
		Embed:      &noopEmbeddingClient{},
	}

	const question = "run the long job that will not fit in three turns"
	resp := h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: sessionID,
		MessageID: "msg_ceiling_1",
		Content:   question,
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	msgs := waitForAssistant(t, h, sessionID, 5*time.Second)
	partial := lastAssistant(msgs)
	// ① the transcript keeps the partial, and keeps it marked.
	if !chat.IsTruncated(partial) {
		t.Fatalf("assistant message is not the marked partial: %q", partial)
	}
	if !strings.Contains(partial, "working on step 3.") {
		t.Errorf("partial lost the last turn's prose; got %q", partial)
	}

	// ② the archive stays empty for this turn: the ceiling error must not be a
	// source of retrievable "knowledge".
	h.Shutdown()
	if err := archive.Load(); err != nil {
		t.Fatalf("archive.Load: %v", err)
	}
	items := archive.Items()
	for _, item := range items {
		if item.Intent == question {
			t.Fatalf("the truncated partial was indexed for retrieval: %+v", item)
		}
	}
	if len(items) != 0 {
		t.Fatalf("expected no archive items from a ceiling run, got %d: %+v", len(items), items)
	}
}

// TestContinueTurn_SeesTheMarkedPartial is the convergence half of Q5-3: "hit the ceiling,
// then say continue" only converges if the continue turn's prompt carries what turn 1
// already did. Run's request is flattenChatMessages(history), so the marked
// partial must appear as an "Assistant:" line in the next request's User field.
// Before the fix the transcript had no assistant message at all, so "continue"
// restarted from zero.
func TestContinueTurn_SeesTheMarkedPartial(t *testing.T) {
	h := newTestChatHandler(t)
	sessionID := "test_ceiling_continue"
	taskID := "chat_" + sessionID

	outBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outBase, taskID), 0755); err != nil {
		t.Fatal(err)
	}
	h.Harness = &chat.ChatHarness{
		Provider:   &ceilingAPIProvider{},
		Model:      "ceilingapi",
		MaxTurns:   2,
		OutputBase: outBase,
	}

	const question = "work through this in several steps"
	h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: sessionID,
		MessageID: "msg_c_1",
		Content:   question,
	})
	msgs := waitForAssistant(t, h, sessionID, 5*time.Second)
	if !chat.IsTruncated(lastAssistant(msgs)) {
		t.Fatalf("turn 1 did not leave a marked partial: %q", lastAssistant(msgs))
	}

	// Turn 2: the operator says continue. Capture what the model is actually shown.
	cap2 := &captureProvider{}
	h.Harness.Provider = cap2
	h.Harness.MaxTurns = 1
	h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: sessionID,
		MessageID: "msg_c_2",
		Content:   "继续",
	})
	time.Sleep(400 * time.Millisecond)
	h.Shutdown()

	shown := cap2.getCaptured()
	if shown == "" {
		t.Fatal("the continue turn never reached the provider")
	}
	if !strings.Contains(shown, chat.TruncatedMarkerPrefix) {
		t.Fatalf("continue turn cannot see the truncation marker; provider input: %s", shown)
	}
	if !strings.Contains(shown, "Assistant: "+chat.TruncatedMarkerPrefix) {
		t.Errorf("the partial is not carried as the assistant's previous turn: %s", shown)
	}
	if !strings.Contains(shown, "working on step 2.") {
		t.Errorf("turn 1's work is missing from the continue turn: %s", shown)
	}
}

func lastAssistant(msgs []chat.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i].Content
		}
	}
	return ""
}

func TestProcessMessage_CleanTurnStillIndexed(t *testing.T) {
	// Control for the gate above: a complete turn must still reach both the
	// transcript and the index, or ① and ② could be satisfied by a broken path.
	h := newTestChatHandler(t)
	sessionID := "test_ceiling_clean"

	archive := core.NewContextArchive(filepath.Join(t.TempDir(), "chat_ctx.jsonl"))
	h.Harness = &chat.ChatHarness{
		Provider: &captureProvider{},
		Model:    "capture",
		MaxTurns: 1,
		Archive:  archive,
		Embed:    &noopEmbeddingClient{},
	}

	const question = "a short question with a short answer"
	h.ProcessMessage(context.Background(), ChatRequest{
		SessionID: sessionID,
		MessageID: "msg_clean_1",
		Content:   question,
	})

	waitForAssistant(t, h, sessionID, 5*time.Second)
	h.Shutdown()

	if err := archive.Load(); err != nil {
		t.Fatalf("archive.Load: %v", err)
	}
	// Indexed at least once — the harness's own indexTurn defer and
	// ProcessMessage both write a complete turn, so the count is not pinned
	// here; that the path is live at all is what the ceiling gate must not break.
	found := false
	for _, item := range archive.Items() {
		if item.Intent == question {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the complete turn in the index, got %+v", archive.Items())
	}
}
