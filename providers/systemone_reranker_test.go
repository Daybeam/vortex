package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// ── SystemOneReranker Adapter Tests ────────────────────────────────────────

func TestSystemOneReranker_Rerank_Success(t *testing.T) {
	// Mock /v1/systemone backend that returns scores for each doc_i question.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req systemOneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		// Build answers: echo back a score for each question.
		answers := make(map[string]systemOneAnswer)
		for qID := range req.Questions {
			answers[qID] = systemOneAnswer{
				Type:   "score",
				Score:  float64(len(qID)), // deterministic: "doc_0"→5, "doc_1"→5, etc.
			}
		}

		resp := systemOneResponse{Answers: answers}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		BaseURL: srv.URL,
		Model:   "test-model",
	}
	provider := NewSystemOneProvider(cfg)
	reranker := NewSystemOneReranker(provider)

	docs := []string{"doc A text", "doc B text", "doc C text"}
	scores, err := reranker.Rerank(context.Background(), "test query", docs)
	if err != nil {
		t.Fatalf("Rerank failed: %v", err)
	}
	if len(scores) != 3 {
		t.Fatalf("expected 3 scores, got %d", len(scores))
	}
	// All scores should be non-zero (len("doc_0")=5, len("doc_1")=5, etc.)
	for i, s := range scores {
		if s <= 0 {
			t.Fatalf("score[%d] = %v, expected > 0", i, s)
		}
	}
}

func TestSystemOneReranker_Rerank_EmptyDocuments(t *testing.T) {
	cfg := &config.ProviderConfig{BaseURL: "http://localhost", Model: "test"}
	reranker := NewSystemOneReranker(NewSystemOneProvider(cfg))

	scores, err := reranker.Rerank(context.Background(), "query", nil)
	if err != nil {
		t.Fatalf("expected nil error for empty docs, got %v", err)
	}
	if scores != nil {
		t.Fatalf("expected nil scores for empty docs, got %v", scores)
	}
}

func TestSystemOneReranker_Rerank_BackendError(t *testing.T) {
	// Backend returns HTTP 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{BaseURL: srv.URL, Model: "test"}
	reranker := NewSystemOneReranker(NewSystemOneProvider(cfg))

	scores, err := reranker.Rerank(context.Background(), "query", []string{"doc A", "doc B"})
	if err == nil {
		t.Fatal("expected error on backend 500, got nil")
	}
	if scores != nil {
		t.Fatalf("expected nil scores on error, got %v", scores)
	}
	if !strings.Contains(err.Error(), "systemone reranker") {
		t.Fatalf("expected wrapped error, got: %v", err)
	}
}

func TestSystemOneReranker_Rerank_CloudflareEnvelope(t *testing.T) {
	// Mock Cloudflare Workers AI endpoint (envelope-wrapped).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cloudflare wraps the request in {model, input}.
		var envelope cloudflareRequest
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if envelope.Model == "" {
			t.Fatal("expected envelope model field")
		}
		if len(envelope.Input.Questions) == 0 {
			t.Fatal("expected questions in envelope input")
		}

		answers := make(map[string]systemOneAnswer)
		for qID := range envelope.Input.Questions {
			answers[qID] = systemOneAnswer{Type: "score", Score: 7.0}
		}
		resp := systemOneResponse{Answers: answers}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		BaseURL: srv.URL,
		Model:   "typesafe/jev",
		Extra:   map[string]any{"envelope": "cloudflare"},
	}
	reranker := NewSystemOneReranker(NewSystemOneProvider(cfg))

	scores, err := reranker.Rerank(context.Background(), "query", []string{"doc A", "doc B"})
	if err != nil {
		t.Fatalf("Rerank with Cloudflare envelope failed: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("expected 2 scores, got %d", len(scores))
	}
	for i, s := range scores {
		if s != 7.0 {
			t.Fatalf("score[%d] = %v, expected 7.0", i, s)
		}
	}
}

func TestSystemOneReranker_Rerank_PartialResponse(t *testing.T) {
	// Backend returns answers for only some questions — missing ones get score 0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req systemOneRequest
		json.NewDecoder(r.Body).Decode(&req)

		answers := make(map[string]systemOneAnswer)
		// Only answer doc_0, skip doc_1 and doc_2.
		answers["doc_0"] = systemOneAnswer{Type: "score", Score: 9.0}

		resp := systemOneResponse{Answers: answers}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{BaseURL: srv.URL, Model: "test"}
	reranker := NewSystemOneReranker(NewSystemOneProvider(cfg))

	scores, err := reranker.Rerank(context.Background(), "query", []string{"A", "B", "C"})
	if err != nil {
		t.Fatalf("Rerank failed: %v", err)
	}
	if scores[0] != 9.0 {
		t.Fatalf("score[0] = %v, expected 9.0", scores[0])
	}
	if scores[1] != 0.0 {
		t.Fatalf("score[1] = %v, expected 0.0 (no answer)", scores[1])
	}
	if scores[2] != 0.0 {
		t.Fatalf("score[2] = %v, expected 0.0 (no answer)", scores[2])
	}
}

func TestDocQuestionID_RoundTrip(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := docQuestionID(i)
		parsed, ok := parseDocQuestionID(id)
		if !ok {
			t.Fatalf("parseDocQuestionID(%q) failed", id)
		}
		if parsed != i {
			t.Fatalf("round-trip failed: %d → %q → %d", i, id, parsed)
		}
	}
}

func TestParseDocQuestionID_Invalid(t *testing.T) {
	invalid := []string{"", "doc", "doc_", "doc_abc", "doc_-1", "other_0"}
	for _, s := range invalid {
		_, ok := parseDocQuestionID(s)
		if ok {
			t.Fatalf("expected parseDocQuestionID(%q) to fail", s)
		}
	}
}
