package core

import (
	"testing"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// ─── Embedding ledger (ADDED 2026-10-12) ───────────────────────────────────

func TestRecordEmbeddingTracksModelAndProvider(t *testing.T) {
	stats := GetGlobalStats()
	// Global singleton: assert deltas so unrelated/parallel activity in the
	// same process cannot distort absolute expectations.
	beforeModel := stats.EmbeddingInputTokens["text-embedding-3-small"]
	beforeProvider := stats.EmbeddingInputTokens["openai"]
	beforeCalls := stats.EmbeddingCalls["text-embedding-3-small"]

	stats.RecordEmbedding("openai", "text-embedding-3-small", 120)
	stats.RecordEmbedding("openai", "text-embedding-3-small", 80)

	if got := stats.EmbeddingInputTokens["text-embedding-3-small"] - beforeModel; got != 200 {
		t.Fatalf("model input-token delta = %d, want 200", got)
	}
	if got := stats.EmbeddingInputTokens["openai"] - beforeProvider; got != 200 {
		t.Fatalf("provider input-token delta = %d, want 200", got)
	}
	if got := stats.EmbeddingCalls["text-embedding-3-small"] - beforeCalls; got != 2 {
		t.Fatalf("model call-count delta = %d, want 2", got)
	}

	// The /api/usage snapshot must expose the ledger under "_embedding".
	snap := stats.GetSnapshot()
	block, ok := snap["_embedding"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot[_embedding] missing or wrong type: %T", snap["_embedding"])
	}
	entry, ok := block["text-embedding-3-small"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot[_embedding] missing model entry for text-embedding-3-small")
	}
	if got := entry["input_tokens"].(int64); got < beforeModel+200 {
		t.Fatalf("snapshot input_tokens = %d, want >= %d", got, beforeModel+200)
	}
	if got := entry["calls"].(int64); got < beforeCalls+2 {
		t.Fatalf("snapshot calls = %d, want >= %d", got, beforeCalls+2)
	}
}

func TestRecordEmbeddingSameModelAndProviderCountsOnce(t *testing.T) {
	stats := GetGlobalStats()
	// Unique key so the exact-value assertions below are deterministic.
	const key = "embedding_selftest_same_key"
	stats.RecordEmbedding(key, key, 30)
	if got := stats.EmbeddingInputTokens[key]; got != 30 {
		t.Fatalf("EmbeddingInputTokens[%s] = %d, want exactly 30 (no double count)", key, got)
	}
	if got := stats.EmbeddingCalls[key]; got != 1 {
		t.Fatalf("EmbeddingCalls[%s] = %d, want exactly 1", key, got)
	}
}

func TestRecordEmbeddingEmptyKeysIgnored(t *testing.T) {
	stats := GetGlobalStats()
	stats.RecordEmbedding("", "", 100)
	if _, ok := stats.EmbeddingInputTokens[""]; ok {
		t.Fatal("empty model/provider key must not be recorded")
	}
}

// ─── Outer ring (ADDED 2026-10-12) ─────────────────────────────────────────

func TestUpdateOuterRingNilSafeAndApplies(t *testing.T) {
	pa := &PromptAssembler{}
	// No profile captured yet: must not panic.
	pa.UpdateOuterRing(10, 20, 30, 40)

	pa.setLastProfile(&ContextProfile{SystemTokens: 100})
	pa.UpdateOuterRing(500, 600, 700, 800)
	got := pa.LastProfile()
	if got == nil {
		t.Fatal("LastProfile() = nil after setLastProfile")
	}
	if got.ToolsTokens != 500 || got.HistoryTokens != 600 || got.PromptTokens != 700 || got.CompletionTokens != 800 {
		t.Fatalf("outer ring = tools:%d history:%d prompt:%d completion:%d, want 500/600/700/800",
			got.ToolsTokens, got.HistoryTokens, got.PromptTokens, got.CompletionTokens)
	}
}

func TestEstimateMCPServersTokens(t *testing.T) {
	servers := []providers.MCPServerDef{
		{
			Name: "_core",
			Tools: []schemas.ToolDefinition{
				{
					Name:        "write_file",
					Description: "Write a file to disk",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}
	if got := estimateMCPServersTokens(servers); got <= 0 {
		t.Fatalf("estimateMCPServersTokens(servers) = %d, want > 0", got)
	}
	if got := estimateMCPServersTokens(nil); got != 0 {
		t.Fatalf("estimateMCPServersTokens(nil) = %d, want 0", got)
	}
}
