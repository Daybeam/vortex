package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// mockEmbeddingClient implements EmbeddingClient for testing without real network calls.
type mockEmbeddingClient struct {
	vec []float32
	err error
}

func (m *mockEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.vec, nil
}

func makeTestRegistry() *config.Registry {
	reg := &config.Registry{
		SOPs: make(map[string]*schemas.SOP),
	}
	reg.SOPs["code-review"] = &schemas.SOP{
		ID:          "code-review",
		Version:     "1.0",
		Description: "Perform code review and suggest improvements",
		Triggers:    []schemas.SOPTrigger{{Keywords: []string{"review", "code", "audit", "refactor"}}},
		Steps: map[string]schemas.SOPStep{
			"step-1": {
				ID:   "step-1",
				Role: "software_developer",
				Task: "Review the code and provide feedback",
			},
		},
	}
	return reg
}

func TestIntentRouter_Route_EmptyClient_Degraded(t *testing.T) {
	reg := makeTestRegistry()
	router := NewIntentRouter(nil)
	router.RefreshEmbeddings(context.Background(), reg)

	result, err := router.Route(context.Background(), reg, "review this code", 3)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if len(result) == 0 || result[0].SOP.ID != "code-review" {
		t.Errorf("Expected keyword fallback match with nil client, got %d results", len(result))
	}
}

func TestIntentRouter_Route_EmbeddingFails_KeywordFallback(t *testing.T) {
	reg := makeTestRegistry()
	client := &mockEmbeddingClient{err: context.DeadlineExceeded}
	router := NewIntentRouter(client)
	router.RefreshEmbeddings(context.Background(), reg)

	result, err := router.Route(context.Background(), reg, "Please review this code", 3)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	if len(result) == 0 {
		t.Fatalf("Expected keyword fallback to find code-review SOP")
	}

	if result[0].SOP.ID != "code-review" {
		t.Errorf("Expected SOP code-review, got %s (mode: %s)", result[0].SOP.ID, result[0].MatchMode)
	}
}

// FIX (eval, 2026-10-13): regression for the drained-embedChan deadlock in
// Route. When the outer select fires on keywordChan FIRST and keyword
// matching finds NO candidates, the old tail select re-waited on embedChan —
// but the embedding racer sends at most one value ever, so if it had already
// delivered inside the 250ms window the channel was permanently drained and
// Route blocked until ctx cancellation (the MCP tool-call ctx never cancels).
// Symptom seen live: orchestrator_submit_task hung ~50% of probe runs on the
// implicit single-step path. The outer select picks uniformly among ready
// cases, hence the repetition below.
func TestIntentRouter_Route_NoKeywordMatch_NoClient_Returns(t *testing.T) {
	reg := makeTestRegistry()
	router := NewIntentRouter(nil)
	router.RefreshEmbeddings(context.Background(), reg)

	// "zzz quux" shares no token with the SOP trigger keywords.
	for i := 0; i < 40; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			res, err := router.Route(context.Background(), reg, "zzz quux completely unrelated input", 3)
			if err == nil && len(res) > 0 {
				t.Errorf("iter %d: expected no results, got %d", i, len(res))
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: Route deadlocked on the no-keyword-match path", i)
		}
	}
}

// blockingEmbeddingClient models an embedding provider that answers slowly.
type blockingEmbeddingClient struct{ release chan struct{} }

func (b *blockingEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	select {
	case <-b.release:
		return []float32{1, 0}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Regression test (2026-10-31): the web edition warmed all 87 SOP embeddings
// synchronously at startup, so a slow provider added ~11s before the HTTP
// listener came up. RefreshEmbeddingsAsync must return immediately and fill
// the cache in the background.
func TestIntentRouter_RefreshEmbeddingsAsync_DoesNotBlock(t *testing.T) {
	reg := makeTestRegistry()
	client := &blockingEmbeddingClient{release: make(chan struct{})}
	router := NewIntentRouter(client)

	started := time.Now()
	router.RefreshEmbeddingsAsync(reg)
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("RefreshEmbeddingsAsync blocked for %v", elapsed)
	}

	close(client.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		router.mu.RLock()
		ready := len(router.embeddings) == len(reg.SOPs)
		router.mu.RUnlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background warm-up did not populate embeddings")
}
