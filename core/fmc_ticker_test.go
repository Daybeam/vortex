package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// FMCBatchTicker (core/fmc_ticker.go) periodically runs
// ExperienceStore.RunFMCBatch. These tests exercise the ticker's own logic
// (opt-in gating, provider resolution, error handling) via runOnce called
// directly, rather than waiting on a real time.Ticker -- keeping the tests
// fast and deterministic.

func newFMCTestExperienceStore(t *testing.T) *store.ExperienceStore {
	t.Helper()
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es, err := store.NewExperienceStore(tmpDir, ts, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}
	return es
}

func TestFMCBatchTicker_Start_NoOpWhenWeakModelUnconfigured(t *testing.T) {
	reg := &config.Registry{}
	es := newFMCTestExperienceStore(t)
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	defer logger.Close()

	ticker := NewFMCBatchTicker(reg, config.ExternalRuntimes{}, es, logger)
	// Start() with no FMCWeakModel configured must not spawn the loop
	// goroutine at all. There's no direct way to observe "no goroutine was
	// spawned" from outside, so this test instead confirms Stop() is safe
	// to call even though Start() never actually started anything (would
	// panic on a close of an already-closed/never-created channel if the
	// no-op guard were missing).
	ticker.Start()
	ticker.Stop() // must not panic
}

func TestFMCBatchTicker_RunOnce_SkipsWhenProviderNotFound(t *testing.T) {
	reg := &config.Registry{
		System:    config.SystemSettings{FMCWeakModel: "nonexistent-provider"},
		Providers: map[string]*config.ProviderConfig{},
	}
	es := newFMCTestExperienceStore(t)
	logDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	defer logger.Close()

	ticker := NewFMCBatchTicker(reg, config.ExternalRuntimes{}, es, logger)
	ticker.runOnce() // must not panic; provider genuinely doesn't exist

	logger.Close()
	events, err := logger.ReadTaskLogs("")
	if err != nil {
		t.Fatalf("ReadTaskLogs: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev["event"] == "EventFMCBatchSkipped" {
			found = true
		}
	}
	if !found {
		t.Error("expected EventFMCBatchSkipped to be logged when the configured provider is not registered")
	}
}

func TestFMCBatchTicker_RunOnce_SkipsWhenExpStoreIsNotConcrete(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{FMCWeakModel: "weak"},
		Providers: map[string]*config.ProviderConfig{
			"weak": {Provider: "openai", Model: "gpt-4o-mini", BaseURL: "http://unused"},
		},
	}
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	defer logger.Close()

	// A fake IExperienceStore (not the concrete *store.ExperienceStore type)
	// must be handled gracefully, not panic on the type assertion.
	fake := &fakeExperienceStore{}
	ticker := NewFMCBatchTicker(reg, config.ExternalRuntimes{}, fake, logger)
	ticker.runOnce() // must not panic

	logger.Close()
	events, _ := logger.ReadTaskLogs("")
	found := false
	for _, ev := range events {
		if ev["event"] == "EventFMCBatchSkipped" {
			found = true
		}
	}
	if !found {
		t.Error("expected EventFMCBatchSkipped when expStore is not a *store.ExperienceStore")
	}
}

func TestFMCBatchTicker_RunOnce_CompletesAgainstRealProvider(t *testing.T) {
	// A minimal OpenAI-compatible mock server that always returns a
	// classification-shaped response, so RunFMCBatch's per-node LLM call
	// succeeds without any real network access.
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"failure_mode":"tool_selection_error","critique":"wrong tool chosen"}`}},
			},
		})
	}))
	defer mockServer.Close()

	reg := &config.Registry{
		System: config.SystemSettings{FMCWeakModel: "weak"},
		Providers: map[string]*config.ProviderConfig{
			"weak": {Provider: "openai", Model: "gpt-4o-mini", BaseURL: mockServer.URL, APIKey: "test-key"},
		},
	}
	es := newFMCTestExperienceStore(t)
	es.Nodes["n1"] = &store.ExperienceNode{
		NodeID:     "n1",
		Capability: "coding",
		Outcome:    "failure",
		Strategy:   "tried the wrong tool",
		// FailureMode intentionally empty -- this is the "pending
		// classification" state RunFMCBatch is meant to backfill.
	}
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	defer logger.Close()

	ticker := NewFMCBatchTicker(reg, config.ExternalRuntimes{}, es, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctx
	ticker.runOnce()

	logger.Close()
	events, err := logger.ReadTaskLogs("")
	if err != nil {
		t.Fatalf("ReadTaskLogs: %v", err)
	}
	completed := false
	for _, ev := range events {
		if ev["event"] == "EventFMCBatchCompleted" {
			completed = true
		}
		if ev["event"] == "EventFMCBatchFailed" {
			t.Fatalf("did not expect EventFMCBatchFailed, got detail: %v", ev["detail"])
		}
	}
	if !completed {
		t.Fatal("expected EventFMCBatchCompleted to be logged")
	}
}
