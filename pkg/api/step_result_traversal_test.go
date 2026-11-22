package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

// mockTaskRegistryForTraversal implements store.TaskRegistry to return a
// graph with OutputFiles containing a path-traversal entry.
type mockTaskRegistryForTraversal struct {
	graphJSON []byte
}

func (m *mockTaskRegistryForTraversal) SaveTask(ctx context.Context, taskID, status string, graphJSON []byte) error {
	return nil
}
func (m *mockTaskRegistryForTraversal) UpdateTaskStatus(ctx context.Context, taskID, status string) error {
	return nil
}
func (m *mockTaskRegistryForTraversal) LoadTask(ctx context.Context, taskID string) (*store.TaskRow, []byte, error) {
	return &store.TaskRow{TaskID: taskID, Status: "running", UpdatedAt: time.Now().Format(time.RFC3339)}, m.graphJSON, nil
}
func (m *mockTaskRegistryForTraversal) ListTasks(ctx context.Context, limit int) ([]store.TaskRow, error) {
	return nil, nil
}

// TestSH3_StepResultHandler_RejectsPathTraversal is a regression test for
// audit S-H3: StepResultHandler must skip output file paths containing ".."
// segments to prevent path traversal attacks via crafted output_file entries.
//
// This test calls the REAL StepResultHandler with a real DirectedEngine whose
// TaskRegistry returns a graph containing a traversal path. A secret file is
// placed at the traversal target — without the fix, the handler would read and
// return its contents. With the fix, the traversal path is skipped.
//
// Reproduction: a path with "../" must be filtered out by StepResultHandler.
func TestSH3_StepResultHandler_RejectsPathTraversal(t *testing.T) {
	// 1. Create a secret file that the traversal path would resolve to.
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "secret.txt")
	secretContent := "SECRET_CONTENT_SHOULD_NOT_LEAK"
	if err := os.WriteFile(secretPath, []byte(secretContent), 0644); err != nil {
		t.Fatalf("failed to write secret file: %v", err)
	}

	// 2. Create a traversal path that resolves to the secret file.
	//    We must construct it WITHOUT filepath.Join because Join cleans ".." out.
	//    The resulting path contains ".." which the S-H3 fix must filter.
	subDir := filepath.Join(tmpDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	// Manual construction: "<subDir>/../secret.txt" — retains the ".." segment.
	traversalPath := subDir + string(filepath.Separator) + ".." + string(filepath.Separator) + "secret.txt"

	// Verify the traversal path actually resolves to the secret file.
	data, err := os.ReadFile(traversalPath)
	if err != nil || string(data) != secretContent {
		t.Fatalf("traversal path setup is wrong: expected %q, got %q, err=%v", secretContent, string(data), err)
	}

	// 3. Build a TaskGraph with OutputFiles containing the traversal path.
	graph := &schemas.TaskGraph{
		TaskID: "task-traversal-test",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
		OutputFiles: []schemas.OutputFile{
			{
				StepID:    "s1",
				Path:      traversalPath,
				Format:    "text",
				CreatedAt: time.Now(),
			},
		},
	}
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepOK}

	graphJSON, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("failed to marshal graph: %v", err)
	}

	// 4. Create a DirectedEngine with a mock TaskRegistry that returns the graph.
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger, err := obs.NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	defer scheduler.Stop()
	scheduler.SetTaskRegistry(&mockTaskRegistryForTraversal{graphJSON: graphJSON})

	// 5. Create an APIHandler and call StepResultHandler.
	app := &tools.App{Scheduler: scheduler}
	handler := NewAPIHandler(app)

	req := httptest.NewRequest("GET", "/api/tasks/task-traversal-test/steps/s1/result", nil)
	rr := httptest.NewRecorder()
	handler.StepResultHandler(rr, req)

	// 6. Verify the response does NOT contain the secret content.
	body := rr.Body.String()
	if contains(body, secretContent) {
		t.Errorf("response leaked secret content via path traversal!\nBody: %s", body)
	}

	// The handler should return a "no output file" response since the traversal
	// path was skipped.
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err == nil {
		if ok, _ := resp["ok"].(bool); ok {
			t.Errorf("expected ok=false (traversal filtered), but got ok=true")
		}
	}
}

// TestSH3_StepResultHandler_AllowsSafePath verifies that safe paths (without
// traversal) are still served correctly by StepResultHandler.
func TestSH3_StepResultHandler_AllowsSafePath(t *testing.T) {
	tmpDir := t.TempDir()
	safePath := filepath.Join(tmpDir, "output.txt")
	safeContent := `{"result": "safe data"}`
	if err := os.WriteFile(safePath, []byte(safeContent), 0644); err != nil {
		t.Fatalf("failed to write safe file: %v", err)
	}

	graph := &schemas.TaskGraph{
		TaskID: "task-safe-path",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
		OutputFiles: []schemas.OutputFile{
			{
				StepID:    "s1",
				Path:      safePath,
				Format:    "json",
				CreatedAt: time.Now(),
			},
		},
	}
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepOK}

	graphJSON, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("failed to marshal graph: %v", err)
	}

	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger, err := obs.NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	defer scheduler.Stop()
	scheduler.SetTaskRegistry(&mockTaskRegistryForTraversal{graphJSON: graphJSON})

	app := &tools.App{Scheduler: scheduler}
	handler := NewAPIHandler(app)

	req := httptest.NewRequest("GET", "/api/tasks/task-safe-path/steps/s1/result", nil)
	rr := httptest.NewRecorder()
	handler.StepResultHandler(rr, req)

	body := rr.Body.String()
	if !contains(body, "safe data") {
		t.Errorf("safe path should be served, but response doesn't contain expected data.\nBody: %s", body)
	}
}

// contains is a simple string contains check (avoids importing strings just
// for this — keeps the test self-contained).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
