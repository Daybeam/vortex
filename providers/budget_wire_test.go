package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// These tests exist because the unit test for classifyBadRequest only proves
// the sniffer works — it says nothing about whether the seven 400 branches in
// openai/anthropic/gemini/ollama actually call it. D3's whole failure mode was
// a wiring gap between "the body says budget" and "the error type says bad
// request", so the wiring is what has to be exercised: a real HTTP 400 through
// the real provider, not a helper call.
//
// 400 twice: StreamComplete is the path the chat loop uses (pkg/chat's
// streamTurn) and Complete is the path the DAG spooler's one-shot calls use.
// Both used to collapse into `bad request:`.

func budgetRefusalServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(budgetBody))
	}))
}

func newTestOpenAIProvider(server *httptest.Server) *OpenAIProvider {
	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	return &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}
}

func TestOpenAIProvider_400BudgetBody_StreamComplete(t *testing.T) {
	server := budgetRefusalServer(t)
	defer server.Close()
	p := newTestOpenAIProvider(server)

	_, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "test", User: "hi", MaxTokens: 100,
	}, func(string) error { return nil })

	var budgetErr *BudgetExhaustedError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected *BudgetExhaustedError from the wire, got %T: %v", err, err)
	}
	// And the D3 symptom itself: the text must not blame the request.
	if err != nil && strings.Contains(err.Error(), "bad request:") {
		t.Fatalf("wire error still reads as a malformed request: %q", err.Error())
	}
}

func TestOpenAIProvider_400BudgetBody_Complete(t *testing.T) {
	server := budgetRefusalServer(t)
	defer server.Close()
	p := newTestOpenAIProvider(server)

	_, err := p.Complete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "test", User: "hi", MaxTokens: 100,
	})

	var budgetErr *BudgetExhaustedError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected *BudgetExhaustedError from the wire, got %T: %v", err, err)
	}
}

// The eighth call site is the real risk: the next 400 branch someone writes
// will happily build a BadRequestError by hand and silently reopen D3. One
// concept, one path -- every 400 goes through classifyBadRequest.
func TestNoProviderBuildsBadRequestDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// The two files allowed to name the type: its own definition and the
	// classifier that decides between it and BudgetExhaustedError.
	allowed := map[string]bool{"types.go": true, "error_body.go": true}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		checked++
		if strings.Contains(string(src), "&BadRequestError{") {
			t.Errorf("%s builds a BadRequestError directly; route the 400 through classifyBadRequest instead", f)
		}
	}
	if checked == 0 {
		t.Fatal("no provider files scanned -- the guard would pass vacuously")
	}
}

// The other half of the split: a context-overflow 400 must keep its old type,
// because core/spawner_utils.go's IsContextLimitError and the step-level
// overflow recovery both key off it.
func TestOpenAIProvider_400ContextOverflow_StaysBadRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(contextBody))
	}))
	defer server.Close()
	p := newTestOpenAIProvider(server)

	_, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model: "gpt-4", System: "test", User: "hi", MaxTokens: 100,
	}, func(string) error { return nil })

	var badReqErr *BadRequestError
	if !errors.As(err, &badReqErr) {
		t.Fatalf("context overflow must stay *BadRequestError, got %T: %v", err, err)
	}
	var budgetErr *BudgetExhaustedError
	if errors.As(err, &budgetErr) {
		t.Fatal("context overflow must not classify as an exhausted budget")
	}
}
