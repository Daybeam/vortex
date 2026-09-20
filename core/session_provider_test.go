package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestSpawner_SessionProviderFallback(t *testing.T) {
	// 1. Setup Registry with NO models (minimal)
	reg := &config.Registry{
		DefaultProvider: "global_main",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"test_role": {
				ID: "test_role",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
		},
	}

	// 2. Setup TaskStore
	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)

	taskID := "task_session"
	stepID := "step_1"

	// 3. Setup TaskGraph with Session Provider
	ephemeralProvider := &config.ProviderConfig{
		Provider:  "openai",
		Model:     "ephemeral-gpt-4o",
		APIKeyEnv: "SESSION_KEY",
		PoolID:    "session_main",
	}

	graph := &schemas.TaskGraph{
		TaskID:         taskID,
		MainProviderID: "session_main",
		SessionProviders: map[string]any{
			"session_main": ephemeralProvider,
		},
		Steps: make(map[string]*schemas.Step),
	}

	// 4. Setup Logger
	logDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})

	// 5. Setup Spawner
	s := NewSpawner(reg, ts, nil, logger, nil, "outputs")

	// 6. Spawn with Session IR in Hub
	ctx := context.Background()
	hub := NewContextHub(reg, graph, nil)

	req := &SpawnRequest{
		TaskID: taskID,
		StepID: stepID,
		RoleID: "test_role",
		Task:   "Test session provider resolution",
		Hub:    hub,
	}

	// This should successfully plan the fallback to session_main
	_, _ = s.doSpawn(ctx, req)

	logger.Close()

	// 7. Verify Logs
	date := time.Now().UTC().Format("2006-01-02")
	logFile := filepath.Join(logDir, date, taskID+".jsonl")
	content, _ := os.ReadFile(logFile)
	logStr := string(content)

	if !strings.Contains(logStr, "EventSessionMainFallbackAdded") {
		t.Errorf("expected EventSessionMainFallbackAdded in logs")
	}
	if !strings.Contains(logStr, "session_main") {
		t.Errorf("expected session_main to be mentioned in logs")
	}
	if !strings.Contains(logStr, "ephemeral-gpt-4o") {
		// EventProviderCallStarted should mention the model resolved from session provider
		t.Errorf("expected ephemeral-gpt-4o to be resolved from session provider")
	}
}

func TestSpawner_OrdinaryTaskFallback(t *testing.T) {
	providers.ClearCache()

	// Mock server that returns 401 for the broken provider
	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, `{"error": {"message": "Invalid API key", "type": "invalid_request_error"}}`)
	}))
	defer brokenSrv.Close()

	// Mock server that returns a valid SSE streaming response for the fallback provider
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {\"content\": \"done\"}}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n")
		fmt.Fprintln(w, "data: [DONE]")
		w.(http.Flusher).Flush()
	}))
	defer okSrv.Close()

	// 1. Setup Registry
	reg := &config.Registry{
		DefaultProvider: "global_main",
		Providers: map[string]*config.ProviderConfig{
			"broken_provider": {
				Provider: "openai",
				Model:    "gpt-3.5-turbo",
				BaseURL:  brokenSrv.URL,
				APIKey:   "fake-broken",
			},
			"global_main": {
				Provider: "openai",
				Model:    "gpt-4o",
				BaseURL:  okSrv.URL,
				APIKey:   "fake-ok",
			},
		},
		Roles: map[string]*config.Role{
			"test_role": {
				ID:       "test_role",
				Provider: "broken_provider",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
		},
	}

	// 2. Setup TaskStore
	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)

	taskID := "task_fallback"
	stepID := "step_1"

	// 3. Setup Spawner
	logDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	s := NewSpawner(reg, ts, nil, logger, nil, "outputs")

	// 4. Spawn
	ctx := context.Background()
	hub := NewContextHub(reg, nil, nil)

	req := &SpawnRequest{
		TaskID: taskID,
		StepID: stepID,
		RoleID: "test_role",
		Task:   "Test ordinary fallback to main",
		Hub:    hub,
	}

	// First call (broken_provider) will fail, then it should fallback to global_main
	_, _ = s.doSpawn(ctx, req)

	logger.Close()

	// 5. Verify Logs
	date := time.Now().UTC().Format("2006-01-02")
	logFile := filepath.Join(logDir, date, taskID+".jsonl")
	content, _ := os.ReadFile(logFile)
	logStr := string(content)

	// It should plan the fallback
	if !strings.Contains(logStr, "EventSessionMainFallbackAdded") {
		t.Errorf("expected EventSessionMainFallbackAdded in logs")
	}

	// It should show a provider fallback event after the first one fails
	if !strings.Contains(logStr, "provider_fallback") {
		t.Errorf("expected provider_fallback event in logs")
	}

	if !strings.Contains(logStr, "global_main") {
		t.Errorf("expected global_main to be mentioned in logs as fallback destination")
	}
}
