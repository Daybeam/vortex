package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/tools"
)

// TestObservabilityEventsHandler_ReturnsFilteredEvents verifies that the
// observability events endpoint correctly filters by event_type and returns
// events in the expected JSON structure.
//
// Regression test for the unified observability panel (ADDED 2026-10-03).
// Audit ID: OBS-001
func TestObservabilityEventsHandler_ReturnsFilteredEvents(t *testing.T) {
	// Create a logger in a temp directory
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Close()

	// Log some HTTP request events
	logger.Log(obs.EventHTTPRequest, "", "", map[string]any{
		"method": "GET", "path": "/api/test", "status": 200,
	})
	logger.Log(obs.EventHTTPRequest, "", "", map[string]any{
		"method": "POST", "path": "/api/submit", "status": 201,
	})
	logger.Log(obs.EventStepStarted, "task_123", "step_0", map[string]any{
		"role": "analyst",
	})

	// Give the async writer time to flush
	time.Sleep(200 * time.Millisecond)

	// Create handler
	app := &tools.App{Logger: logger}
	handler := &APIHandler{App: app}

	// Test: filter by http_request event type
	req := httptest.NewRequest("GET", "/api/observability/events?event_type=http_request&limit=10", nil)
	rr := httptest.NewRecorder()
	handler.ObservabilityEventsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	events, ok := resp["events"].([]any)
	if !ok {
		t.Fatalf("expected events array, got %T", resp["events"])
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 HTTP request events, got %d", len(events))
	}

	// Verify both events are http_request type
	for _, ev := range events {
		evMap := ev.(map[string]any)
		if evMap["event"] != "http_request" {
			t.Errorf("expected event type http_request, got %v", evMap["event"])
		}
	}
}

// TestObservabilityStatsHandler_ReturnsCounts verifies that the stats
// endpoint correctly counts events by type.
//
// Regression test for the unified observability panel (ADDED 2026-10-03).
// Audit ID: OBS-002
func TestObservabilityStatsHandler_ReturnsCounts(t *testing.T) {
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Close()

	// Log events of different types
	logger.Log(obs.EventHTTPRequest, "", "", map[string]any{"method": "GET"})
	logger.Log(obs.EventHTTPRequest, "", "", map[string]any{"method": "POST"})
	logger.Log(obs.EventHTTPRequest, "", "", map[string]any{"method": "GET"})
	logger.Log(obs.EventProviderCallStarted, "task_1", "step_0", map[string]any{"provider": "gemini"})
	logger.Log(obs.EventToolCall, "task_1", "step_0", map[string]any{"tool": "write_file"})

	time.Sleep(200 * time.Millisecond)

	app := &tools.App{Logger: logger}
	handler := &APIHandler{App: app}

	req := httptest.NewRequest("GET", "/api/observability/stats", nil)
	rr := httptest.NewRecorder()
	handler.ObservabilityStatsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var stats map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	httpReqs, _ := stats["http_requests"].(float64)
	if httpReqs != 3 {
		t.Errorf("expected 3 http_requests, got %v", httpReqs)
	}

	providerCalls, _ := stats["provider_calls"].(float64)
	if providerCalls != 1 {
		t.Errorf("expected 1 provider_call, got %v", providerCalls)
	}

	toolCalls, _ := stats["tool_calls"].(float64)
	if toolCalls != 1 {
		t.Errorf("expected 1 tool_call, got %v", toolCalls)
	}
}

// TestHTTPLoggingMiddleware_LogsRequests verifies that the HTTP logging
// middleware captures method, path, and status code.
//
// Regression test for the unified observability panel (ADDED 2026-10-03).
// Audit ID: OBS-003
func TestHTTPLoggingMiddleware_LogsRequests(t *testing.T) {
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Close()

	// Create a simple handler to wrap
	innerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))
	})

	wrapped := HTTPLoggingMiddleware(logger)(innerHandler)

	req := httptest.NewRequest("POST", "/api/test", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.Code)
	}

	// Give the async writer time to flush
	time.Sleep(200 * time.Millisecond)

	// Verify the event was logged
	events, _ := logger.ReadAllLogs("", "http_request", "", 10, 0)
	if len(events) != 1 {
		t.Fatalf("expected 1 logged event, got %d", len(events))
	}

	detail := events[0]["detail"].(map[string]any)
	if detail["method"] != "POST" {
		t.Errorf("expected method POST, got %v", detail["method"])
	}
	if detail["path"] != "/api/test" {
		t.Errorf("expected path /api/test, got %v", detail["path"])
	}
	if detail["status"] != float64(http.StatusCreated) {
		t.Errorf("expected status 201, got %v", detail["status"])
	}
}
