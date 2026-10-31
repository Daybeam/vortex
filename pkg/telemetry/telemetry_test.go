package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func TestExporter_Sync(t *testing.T) {
	// Setup mock experience store
	ts := &mockTaskStore{}
	sys := &config.SystemSettings{ConfidenceThreshold: 0.7}
	exp, err := store.NewExperienceStore(t.TempDir(), ts, sys, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}

	// Add some data to exp
	exp.Mu.Lock()
	exp.TaskPatterns["test_pattern"] = store.TaskPattern{
		ID:         "test_pattern",
		TaskID:     "real-task-id",
		SourceText: "Secret business logic",
	}
	exp.Mu.Unlock()

	// Setup mock server
	var receivedData map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedData); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Case 1: DetailedFeedback = false (Sanitization check)
	cfg := config.TelemetryConfig{
		Enabled:          true,
		Endpoint:         server.URL,
		DetailedFeedback: false,
	}
	exporter := NewExporter(cfg, exp)
	err = exporter.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	patterns := receivedData["task_patterns"].(map[string]any)
	p := patterns["test_pattern"].(map[string]any)
	if p["task_id"] != nil && p["task_id"] != "" {
		t.Errorf("expected TaskID to be empty/nil, got %v", p["task_id"])
	}
	if p["source_text"] != nil && p["source_text"] != "" {
		t.Errorf("expected SourceText to be empty/nil (detailed=false), got %v", p["source_text"])
	}

	// Case 2: DetailedFeedback = true
	cfg.DetailedFeedback = true
	exporter = NewExporter(cfg, exp)
	receivedData = nil
	err = exporter.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	patterns = receivedData["task_patterns"].(map[string]any)
	p = patterns["test_pattern"].(map[string]any)
	if p["source_text"] != "Secret business logic" {
		t.Errorf("expected SourceText to be preserved, got %v", p["source_text"])
	}
}

// TestExporter_Sync_RedactsTypedSnapshotValues is the regression test for G10
// (2026-10-31): the snapshot is made of concrete structs, so the previous
// RedactMap(snapshot) call saw no nested strings and sent SourceText and
// StepSequence values verbatim. Sync now normalizes through JSON before
// redacting.
func TestExporter_Sync_RedactsTypedSnapshotValues(t *testing.T) {
	ts := &mockTaskStore{}
	sys := &config.SystemSettings{ConfidenceThreshold: 0.7}
	exp, err := store.NewExperienceStore(t.TempDir(), ts, sys, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}

	exp.Mu.Lock()
	exp.TaskPatterns["leaky"] = store.TaskPattern{
		ID:         "leaky",
		SourceText: "call it with api_key=supersecret12345",
		StepSequence: []map[string]any{
			{"role_id": "r1", "password": "hunter2secret"},
		},
	}
	exp.Mu.Unlock()

	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := new(strings.Builder)
		if _, err := io.Copy(raw, r.Body); err != nil {
			t.Errorf("read request body: %v", err)
		}
		body = raw.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exporter := NewExporter(config.TelemetryConfig{
		Enabled:          true,
		Endpoint:         server.URL,
		DetailedFeedback: true,
	}, exp)
	if err := exporter.Sync(context.Background()); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	for _, leaked := range []string{"supersecret12345", "hunter2secret"} {
		if strings.Contains(body, leaked) {
			t.Errorf("secret %q leaked to telemetry: %s", leaked, body)
		}
	}
	if !strings.Contains(body, "supe****") {
		t.Errorf("expected the source_text secret to be masked, got %s", body)
	}
	if !strings.Contains(body, "hunt****") {
		t.Errorf("expected the step_sequence secret to be masked, got %s", body)
	}
}

type mockTaskStore struct{}

func (m *mockTaskStore) Set(ctx context.Context, taskID, stepID string, result *store.StepResult) error {
	return nil
}
func (m *mockTaskStore) Get(ctx context.Context, taskID, stepID string) (*store.StepResult, error) {
	return nil, nil
}
func (m *mockTaskStore) GetBatch(ctx context.Context, taskID string, stepIDs []string) (map[string]*store.StepResult, error) {
	return make(map[string]*store.StepResult), nil
}
func (m *mockTaskStore) GetByRef(ctx context.Context, ref string) (*store.StepResult, error) {
	return nil, nil
}
func (m *mockTaskStore) ClearTask(ctx context.Context, taskID string) (int, error) { return 0, nil }
func (m *mockTaskStore) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}
