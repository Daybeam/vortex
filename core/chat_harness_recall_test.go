package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression test for semantic recall — long-conversation reference §4.2.
// Plan: docs/plan/LONG_CONVERSATION_REFERENCE.md
// Bug: chat turns were never indexed in ContextArchive, so SearchForest
// (called in buildSystem) could not recall semantically related past chat
// messages. indexTurn was the missing ~10-line method that bridges chat
// history into the existing archive infrastructure.

type recallMockEmbed struct {
	vec []float32
}

func (m *recallMockEmbed) Embed(ctx context.Context, text string) ([]float32, error) {
	return m.vec, nil
}

type recallSimpleProvider struct{}

func (p *recallSimpleProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *recallSimpleProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	onChunk("Set the lsp.enabled flag in config.json.")
	return &schemas.ProviderResponse{Text: "Set the lsp.enabled flag in config.json."}, nil
}

func (p *recallSimpleProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func (p *recallSimpleProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}

func (p *recallSimpleProvider) Name() string { return "recall-simple" }

func TestIndexTurn_StoresChatTurnInArchive(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "chat_ctx.jsonl"))
	harness := &ChatHarness{
		Archive: archive,
		Embed:   &recallMockEmbed{vec: []float32{0.1, 0.2, 0.3}},
	}

	harness.indexTurn(context.Background(), "sess-1", "how do I configure the LSP server?", "Set the lsp.enabled flag in config.json.")

	if err := archive.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	items := archive.items
	if len(items) != 1 {
		t.Fatalf("expected 1 indexed item, got %d", len(items))
	}
	item := items[0]
	if item.TaskID != "chat_sess-1" {
		t.Errorf("TaskID = %q, want %q", item.TaskID, "chat_sess-1")
	}
	if item.Intent != "how do I configure the LSP server?" {
		t.Errorf("Intent = %q", item.Intent)
	}
	if item.Summary != "Set the lsp.enabled flag in config.json." {
		t.Errorf("Summary = %q", item.Summary)
	}
	if len(item.Embedding) != 3 {
		t.Errorf("Embedding len = %d, want 3", len(item.Embedding))
	}
}

func TestIndexTurn_NilSafe(t *testing.T) {
	harness := &ChatHarness{}
	harness.indexTurn(context.Background(), "s", "q", "a")
}

func TestIndexTurn_SearchForestRecalls(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "chat_ctx.jsonl"))
	embed := &recallMockEmbed{vec: []float32{0.4, 0.5, 0.6}}
	harness := &ChatHarness{
		Archive: archive,
		Embed:   embed,
	}

	harness.indexTurn(context.Background(), "sess-42", "configure LSP server", "enable lsp in config")

	forest, err := archive.SearchForest("how to set up LSP?", embed, 5, 0.9, "", "")
	if err != nil {
		t.Fatalf("SearchForest error: %v", err)
	}
	if len(forest.Nodes) == 0 {
		t.Fatal("SearchForest returned no results; expected recalled chat turn")
	}
	found := false
	for _, n := range forest.Nodes {
		if n.TaskID == "chat_sess-42" {
			found = true
			break
		}
	}
	if !found {
		t.Error("indexed chat turn not found in SearchForest results")
	}
}

// Integration test: Run end-to-end → defer fires indexTurn → archive has the
// turn → SearchForest recalls it. This bridges the unit tests above with the
// actual Run path, verifying the named-return + defer wiring works at runtime.
// Plan: docs/plan/LONG_CONVERSATION_REFERENCE.md §5.3
func TestRun_IndexesChatTurnForSemanticRecall(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "chat_ctx.jsonl"))
	embed := &recallMockEmbed{vec: []float32{0.1, 0.2, 0.3}}

	h := &ChatHarness{
		Provider: &recallSimpleProvider{},
		Model:    "mock",
		Archive:  archive,
		Embed:    embed,
		MaxTurns: 3,
	}

	history := []ChatMessage{
		{Role: "user", Content: "how do I configure the LSP server?"},
	}

	result, err := h.Run(context.Background(), "integration-sess", history, func(ev ChatEvent) error { return nil })
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result == "" {
		t.Fatal("Run returned empty result")
	}

	if err := archive.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.items) != 1 {
		t.Fatalf("expected 1 archived item after Run, got %d", len(archive.items))
	}
	item := archive.items[0]
	if item.TaskID != "chat_integration-sess" {
		t.Errorf("TaskID = %q, want %q", item.TaskID, "chat_integration-sess")
	}
	if item.Intent != "how do I configure the LSP server?" {
		t.Errorf("Intent = %q", item.Intent)
	}

	forest, err := archive.SearchForest("LSP configuration", embed, 5, 0.9, "", "")
	if err != nil {
		t.Fatalf("SearchForest: %v", err)
	}
	if len(forest.Nodes) == 0 {
		t.Fatal("SearchForest returned no results; expected recalled chat turn")
	}
}
