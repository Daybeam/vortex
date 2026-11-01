package core

import "context"

// ContextSearchBackend is the extension point for context archive search (A19).
// The default implementation uses in-memory BM25 + embeddings (core/context_archive.go).
// Future implementations: Qdrant, Milvus, pgvector, etc.
// Medium-volume with embedding vectors (~1536 floats per item).
type ContextSearchBackend interface {
	Search(ctx context.Context, query string, embedClient EmbeddingClient, k int, taskScope, currentTaskID string) []MemoryItem
	SearchForest(ctx context.Context, query []float32, capability, errorSignal string, tokenBudget int) []MemoryItem
	Store(ctx context.Context, item MemoryItem) error
	Load(ctx context.Context) error
}
