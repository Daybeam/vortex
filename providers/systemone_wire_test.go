package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/daybeam/vortex/config"
	"strings"
	"testing"
	"time"
)

// TestSystemOneWireNormalization verifies the 2026-10-14 wire-schema fix:
// legacy builders (tool_router, decision gate, failure_classify) emit
// "question"+"options"; laya-serve >=0.3 requires "instructions"+"criteria".
// The provider must translate at the choke point and append /v1/systemone
// when base_url carries no path.
func TestSystemOneWireNormalization(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"laya-rl-agent","answers":{"q1":{"type":"choice","choice":"b","confidence":0.9,"probabilities":{"a":0.1,"b":0.9}}}}`))
	}))
	defer srv.Close()

	p := NewSystemOneProvider(&config.ProviderConfig{
		Provider: "systemone",
		BaseURL:  srv.URL, // bare host — endpoint append must kick in
	})
	req := CompleteRequest{
		User:      "Task: change flight",
		MaxTokens: 256,
		Constraints: map[string]any{
			"questions": map[string]any{
				"q1": map[string]any{
					"type":     "choice",
					"question": "Which tool is most relevant to this task?",
					"options":  []string{"a", "b"},
				},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("endpoint = %q, want /v1/systemone", gotPath)
	}
	qs, _ := gotBody["questions"].(map[string]any)
	if qs == nil {
		t.Fatalf("no questions in posted body: %v", gotBody)
	}
	q1, _ := qs["q1"].(map[string]any)
	if q1 == nil {
		t.Fatalf("q1 missing: %v", qs)
	}
	if _, ok := q1["instructions"]; !ok {
		t.Errorf("instructions not synthesized from question: %v", q1)
	}
	if _, ok := q1["criteria"]; !ok {
		t.Errorf("criteria not synthesized from options: %v", q1)
	}
	// legacy keys preserved for backends that still speak the old schema
	if _, ok := q1["question"]; !ok {
		t.Errorf("legacy question key dropped: %v", q1)
	}
	if len(resp.ToolCalls) == 0 || resp.ToolCalls[0].Name != "q1" {
		t.Fatalf("unexpected ToolCalls: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Arguments["choice"] != "b" {
		t.Errorf("choice = %v, want b", resp.ToolCalls[0].Arguments["choice"])
	}
}

// TestSystemOneEndpointFullPath verifies a base_url that already contains a
// path is used verbatim (no double-append).
func TestSystemOneEndpointFullPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer srv.Close()

	p := NewSystemOneProvider(&config.ProviderConfig{
		Provider: "systemone",
		BaseURL:  srv.URL + "/v1/systemone",
	})
	req := CompleteRequest{
		User: "x",
		Constraints: map[string]any{
			"questions": map[string]any{
				"q1": map[string]any{"type": "choice", "instructions": "i", "criteria": []string{"a"}},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = p.Complete(ctx, req) // response decode may fail on empty answers; path is what we assert
	if !strings.HasSuffix(gotPath, "/v1/systemone") || strings.Count(gotPath, "/v1/systemone") != 1 {
		t.Errorf("path = %q, want exactly one /v1/systemone suffix", gotPath)
	}
}
