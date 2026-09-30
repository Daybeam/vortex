package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

// newTestSystemOneProvider creates a provider pointing at a test backend.
func newTestSystemOneProvider(t *testing.T, handler http.HandlerFunc, extra map[string]any) (*SystemOneProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.ProviderConfig{
		Provider: "systemone",
		Model:    "jev-latest",
		BaseURL:  srv.URL,
		Extra:    extra,
	}
	return NewSystemOneProvider(cfg), srv
}

// sampleQuestions returns a /v1/systemone questions map matching §0.4.1.
func sampleQuestions() map[string]any {
	return map[string]any{
		"urgency": map[string]any{
			"type":         "noul",
			"instructions": "Is this urgent?",
			"criteria": map[string]string{
				"true":  "The user needs immediate help",
				"false": "This can wait",
			},
		},
		"department": map[string]any{
			"type":         "choice",
			"instructions": "Which department?",
			"criteria": map[string]string{
				"billing":   "Billing questions",
				"technical": "Technical support",
			},
		},
		"frustration": map[string]any{
			"type":         "score",
			"instructions": "How frustrated?",
			"criteria":     []string{"calm", "civil", "angry"},
		},
	}
}

// sampleAnswers returns a /v1/systemone answers map matching §0.4.1.
func sampleAnswers() map[string]systemOneAnswer {
	return map[string]systemOneAnswer{
		"urgency": {
			Type:       "noul",
			Noul:       0.95,
			Confidence: 0.9,
		},
		"department": {
			Type:          "choice",
			Choice:        "billing",
			Confidence:    0.8,
			Probabilities: map[string]float64{"billing": 0.87, "technical": 0.13},
		},
		"frustration": {
			Type:          "score",
			Score:         1.04,
			Confidence:    0.94,
			Legend:        map[string]string{"0": "calm", "1": "civil", "2": "angry"},
			Probabilities: map[string]float64{"0": 0, "1": 0.96, "2": 0.04},
		},
	}
}

func TestSystemOneProvider_Name(t *testing.T) {
	p, _ := newTestSystemOneProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
	if got := p.Name(); got != "systemone:jev-latest" {
		t.Errorf("Name() = %q, want %q", got, "systemone:jev-latest")
	}
}

func TestSystemOneProvider_Complete_HappyPath(t *testing.T) {
	var receivedReq systemOneRequest

	handler := func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Errorf("backend decode: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Verify the backend received the right state + model + questions.
		if receivedReq.State != "customer is angry about billing" {
			t.Errorf("backend got state=%q, want %q", receivedReq.State, "customer is angry about billing")
		}
		if receivedReq.Model != "jev-latest" {
			t.Errorf("backend got model=%q, want %q", receivedReq.Model, "jev-latest")
		}
		if len(receivedReq.Questions) != 3 {
			t.Errorf("backend got %d questions, want 3", len(receivedReq.Questions))
		}

		resp := systemOneResponse{
			Model:   "jev-1.13.0",
			Answers: sampleAnswers(),
			Usage:   &systemOneUsage{InputTokens: 426, OutputTokens: 73},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	p, _ := newTestSystemOneProvider(t, handler, nil)

	req := CompleteRequest{
		User: "customer is angry about billing",
		Constraints: map[string]any{
			"questions": sampleQuestions(),
		},
	}

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if resp.Text != "" {
		t.Errorf("Text = %q, want empty (System One produces no free text)", resp.Text)
	}
	if resp.StopReason != "scored" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "scored")
	}
	if len(resp.ToolCalls) != 3 {
		t.Fatalf("len(ToolCalls) = %d, want 3", len(resp.ToolCalls))
	}

	// Find the "department" ToolCall and verify choice mapping.
	var deptTC *ToolCall
	for i := range resp.ToolCalls {
		if resp.ToolCalls[i].Name == "department" {
			deptTC = &resp.ToolCalls[i]
		}
	}
	if deptTC == nil {
		t.Fatal("no ToolCall named 'department'")
	}
	if deptTC.Arguments["type"] != "choice" {
		t.Errorf("department type = %v, want %q", deptTC.Arguments["type"], "choice")
	}
	if deptTC.Arguments["choice"] != "billing" {
		t.Errorf("department choice = %v, want %q", deptTC.Arguments["choice"], "billing")
	}
	if deptTC.Arguments["confidence"] != 0.8 {
		t.Errorf("department confidence = %v, want 0.8", deptTC.Arguments["confidence"])
	}
}

func TestSystemOneProvider_Complete_AllAnswerTypes(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	p, _ := newTestSystemOneProvider(t, handler, nil)

	resp, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	// Build a map for easy lookup.
	tcMap := make(map[string]ToolCall, len(resp.ToolCalls))
	for _, tc := range resp.ToolCalls {
		tcMap[tc.Name] = tc
	}

	// Verify noul type.
	if tc, ok := tcMap["urgency"]; ok {
		if tc.Arguments["type"] != "noul" {
			t.Errorf("urgency type = %v, want noul", tc.Arguments["type"])
		}
		if tc.Arguments["noul"] != 0.95 {
			t.Errorf("urgency noul = %v, want 0.95", tc.Arguments["noul"])
		}
	} else {
		t.Error("missing urgency ToolCall")
	}

	// Verify score type.
	if tc, ok := tcMap["frustration"]; ok {
		if tc.Arguments["type"] != "score" {
			t.Errorf("frustration type = %v, want score", tc.Arguments["type"])
		}
		if tc.Arguments["score"] != 1.04 {
			t.Errorf("frustration score = %v, want 1.04", tc.Arguments["score"])
		}
		if tc.Arguments["legend"] == nil {
			t.Error("frustration legend should be present")
		}
	} else {
		t.Error("missing frustration ToolCall")
	}
}

func TestSystemOneProvider_Complete_ScoreThresholdFiltering(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		// Only one answer with high confidence, one with low.
		resp := systemOneResponse{
			Answers: map[string]systemOneAnswer{
				"high": {Type: "noul", Noul: 0.9, Confidence: 0.95},
				"low":  {Type: "noul", Noul: 0.1, Confidence: 0.3},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	// score_threshold = 0.5 → only "high" (0.95) survives; "low" (0.3) filtered.
	p, _ := newTestSystemOneProvider(t, handler, map[string]any{
		"score_threshold": 0.5,
	})

	resp, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1 (threshold=0.5 filters low@0.3)", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "high" {
		t.Errorf("ToolCalls[0].Name = %q, want %q", resp.ToolCalls[0].Name, "high")
	}
}

func TestSystemOneProvider_Complete_BearerAuth(t *testing.T) {
	var gotAuth string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	cfg := &config.ProviderConfig{
		Provider: "systemone",
		Model:    "jev-latest",
		BaseURL:  srv.URL,
		APIKey:   "test-key-123",
	}
	p := NewSystemOneProvider(cfg)

	_, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if gotAuth != "Bearer test-key-123" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-key-123")
	}
}

func TestSystemOneProvider_Complete_NoAuthWhenAPIKeyEmpty(t *testing.T) {
	var gotAuth string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	p, _ := newTestSystemOneProvider(t, handler, nil)

	_, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty (no API key)", gotAuth)
	}
}

func TestSystemOneProvider_Complete_CloudflareEnvelope(t *testing.T) {
	var receivedBody map[string]any

	handler := func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	p, _ := newTestSystemOneProvider(t, handler, map[string]any{
		"envelope": "cloudflare",
	})

	_, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	// Cloudflare envelope: {model, input: {state, model, questions}}
	model, ok := receivedBody["model"].(string)
	if !ok || model != "jev-latest" {
		t.Errorf("envelope model = %v, want %q", receivedBody["model"], "jev-latest")
	}
	input, ok := receivedBody["input"].(map[string]any)
	if !ok {
		t.Fatal("envelope should have 'input' map")
	}
	if input["state"] != "state" {
		t.Errorf("envelope input.state = %v, want %q", input["state"], "state")
	}
	if _, ok := input["questions"].(map[string]any); !ok {
		t.Error("envelope input should have questions map")
	}
}

func TestSystemOneProvider_Complete_MissingBaseURL(t *testing.T) {
	cfg := &config.ProviderConfig{
		Provider: "systemone",
		Model:    "test",
	}
	p := NewSystemOneProvider(cfg)

	_, err := p.Complete(context.Background(), CompleteRequest{
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err == nil {
		t.Fatal("Complete() with empty BaseURL should error")
	}
}

func TestSystemOneProvider_Complete_MissingQuestions(t *testing.T) {
	p, _ := newTestSystemOneProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)

	_, err := p.Complete(context.Background(), CompleteRequest{
		User: "state",
		// No Constraints["questions"]
	})
	if err == nil {
		t.Fatal("Complete() without questions should error")
	}
}

func TestSystemOneProvider_Complete_BackendError(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusInternalServerError)
	}
	p, _ := newTestSystemOneProvider(t, handler, nil)

	_, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err == nil {
		t.Fatal("Complete() with HTTP 500 should error")
	}
}

func TestSystemOneProvider_StreamComplete_DelegatesToComplete(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
	p, _ := newTestSystemOneProvider(t, handler, nil)

	chunkCalled := false
	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	}, func(text string) error {
		chunkCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("StreamComplete() error: %v", err)
	}
	if chunkCalled {
		t.Error("onChunk was called, but System One has no streaming semantics")
	}
	if resp.StopReason != "scored" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "scored")
	}
}

func TestSystemOneProvider_Embed_NotSupported(t *testing.T) {
	p, _ := newTestSystemOneProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)

	_, err := p.Embed(context.Background(), "test")
	if err == nil {
		t.Fatal("Embed() should return error")
	}
}

func TestSystemOneProvider_CountTokens(t *testing.T) {
	p, _ := newTestSystemOneProvider(t, func(w http.ResponseWriter, r *http.Request) {}, nil)

	n, err := p.CountTokens(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("CountTokens() error: %v", err)
	}
	if n <= 0 {
		t.Errorf("CountTokens() = %d, want > 0", n)
	}
}

func TestSystemOneProvider_Complete_FallsBackToSystemString(t *testing.T) {
	var receivedState string
	handler := func(w http.ResponseWriter, r *http.Request) {
		var req systemOneRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedState = req.State
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
	p, _ := newTestSystemOneProvider(t, handler, nil)

	_, err := p.Complete(context.Background(), CompleteRequest{
		System:      "system-prompt-state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if receivedState != "system-prompt-state" {
		t.Errorf("backend received state=%q, want %q (fallback to System)", receivedState, "system-prompt-state")
	}
}

func TestSystemOneProvider_FactoryIntegration(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		resp := systemOneResponse{Answers: sampleAnswers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	cfg := &config.ProviderConfig{
		Provider: "systemone",
		Model:    "jev-latest",
		BaseURL:  srv.URL,
	}

	p, err := newProvider(cfg, config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("newProvider() error: %v", err)
	}
	if p.Name() != "systemone:jev-latest" {
		t.Errorf("Name() = %q, want %q", p.Name(), "systemone:jev-latest")
	}

	resp, err := p.Complete(context.Background(), CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if resp.StopReason != "scored" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "scored")
	}
	if len(resp.ToolCalls) != 3 {
		t.Errorf("len(ToolCalls) = %d, want 3", len(resp.ToolCalls))
	}
}

func TestSystemOneProvider_Complete_PropagatesContextCancellation(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		http.Error(w, "cancelled", http.StatusRequestTimeout)
	}
	p, _ := newTestSystemOneProvider(t, handler, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Complete(ctx, CompleteRequest{
		User:        "state",
		Constraints: map[string]any{"questions": sampleQuestions()},
	})
	if err == nil {
		t.Fatal("Complete() with cancelled context should error")
	}
}
