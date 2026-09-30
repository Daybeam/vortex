package providers

import (
	"context"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/store"
)

// ─── System One Reranker Adapter ───────────────────────────────────────────
//
// SystemOneReranker adapts SystemOneProvider to the store.IRerankerClient
// interface. It builds /v1/systemone "score" questions for each candidate
// document, sends them in a single batch, and extracts scores from the
// response. Scores are runtime-only — never persisted (whitepaper §1.3).
//
// Usage:
//
//	reranker := NewSystemOneReranker(systemOneProvider)
//	expStore.SetRerankerClient(reranker)
//
// The adapter is a thin wrapper — all HTTP, envelope, and auth logic lives
// in SystemOneProvider. This separation keeps the wire protocol concerns in
// the provider and the fusion logic in the store.

// SystemOneReranker wraps a SystemOneProvider to implement store.IRerankerClient.
type SystemOneReranker struct {
	provider *SystemOneProvider
}

// NewSystemOneReranker creates a reranker adapter wrapping a System One provider.
func NewSystemOneReranker(p *SystemOneProvider) *SystemOneReranker {
	return &SystemOneReranker{provider: p}
}

// Rerank scores each document for relevance to the query using the System One
// /v1/systemone wire protocol. Each document becomes a "score" question; the
// backend returns a score per question in a single forward pass.
//
// The returned scores are backend-specific (Jev linear vs Laya entropy) and
// must NOT be compared across backends or persisted. Callers convert to
// relative ranks via RRF fusion (whitepaper §1.3, §3.2).
func (r *SystemOneReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return nil, nil
	}

	// Build "score" questions for each candidate document (§0.4.1).
	questions := make(map[string]any, len(documents))
	for i, doc := range documents {
		qID := docQuestionID(i)
		questions[qID] = map[string]any{
			"type":         "score",
			"instructions": "Rate the relevance of this experience to the query on a scale of 0-10",
			"criteria": map[string]any{
				"query": query,
				"text":  doc,
			},
		}
	}

	resp, err := r.provider.Complete(ctx, CompleteRequest{
		User:        query,
		Constraints: map[string]any{"questions": questions},
	})
	if err != nil {
		return nil, fmt.Errorf("systemone reranker: %w", err)
	}

	// Extract scores from ToolCalls. Each ToolCall.Name = "doc_<i>",
	// ToolCall.Arguments["score"] = the numeric score from the backend.
	scores := make([]float64, len(documents))
	for _, tc := range resp.ToolCalls {
		idx, ok := parseDocQuestionID(tc.Name)
		if !ok || idx < 0 || idx >= len(documents) {
			continue
		}
		if score, ok := tc.Arguments["score"].(float64); ok {
			scores[idx] = score
		}
	}

	return scores, nil
}

// docQuestionID builds the question ID for document at index i.
func docQuestionID(i int) string {
	return fmt.Sprintf("doc_%d", i)
}

// parseDocQuestionID extracts the document index from a "doc_<i>" question ID.
func parseDocQuestionID(name string) (int, bool) {
	if !strings.HasPrefix(name, "doc_") {
		return 0, false
	}
	var idx int
	if _, err := fmt.Sscanf(name, "doc_%d", &idx); err != nil {
		return 0, false
	}
	if idx < 0 {
		return 0, false
	}
	return idx, true
}

// Compile-time interface check.
var _ store.IRerankerClient = (*SystemOneReranker)(nil)
