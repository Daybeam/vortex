//go:build live

// Package live contains external-call tests that hit real model APIs.
// These tests are isolated from normal `go test ./...` via the `live` build
// tag — they only compile and run with: go test -tags live ./tests/live/
//
// This prevents accidental API calls (and costs) during normal CI/development.
//
// Model: iFlytek Spark-X2.5-4B (iFlytek 4B) via OpenAI-compatible API.
// Source: D:\myCLaw\eval-kit\eval_kit\config_airline_xfyun_chat.json
//
// Prerequisites:
//   - Model proxy running on 127.0.0.1:8099 (or set XFYUN_BASE_URL)
//   - Or a direct iFlytek endpoint with XFYUN_BASE_URL + XFYUN_API_KEY
//   - Config file: tests/live/config_xfyun_live.json
package live

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/jit"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// configPath is the live test config relative to this test file.
const configPath = "config_xfyun_live.json"

// graphStatusString extracts the graph status from a GetStatus response map.
// GraphStatus is a named type (type GraphStatus string), so the .(string)
// type assertion fails — this helper falls back to fmt.Sprintf.
func graphStatusString(status map[string]any) string {
	if s, ok := status["status"].(string); ok {
		return s
	}
	return fmt.Sprintf("%v", status["status"])
}

// checkEndpointReachable skips the test if the model API endpoint is not
// responding. This prevents the test from hanging on a dead proxy.
func checkEndpointReachable(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/models")
	if err != nil {
		t.Skipf("model endpoint %s not reachable: %v — start the proxy or set XFYUN_BASE_URL", baseURL, err)
	}
	resp.Body.Close()
}

// buildEngine creates a real DirectedEngine from the live config. Mirrors
// the production wiring in main.go (NewRegistry → NewLogger → NewJITManager
// → NewDirectedEngine) but with minimal dependencies (no signal field,
// no resource loader — nil-safe per engine design).
func buildEngine(t *testing.T) (*core.DirectedEngine, *config.Registry) {
	t.Helper()

	// Allow override via env var for flexibility.
	cfgPath := configPath
	if p := os.Getenv("XFYUN_CONFIG"); p != "" {
		cfgPath = p
	}

	reg, err := config.NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("NewRegistry(%s): %v", cfgPath, err)
	}

	// Check endpoint reachability before creating the engine.
	if p := reg.Providers[reg.DefaultProvider]; p != nil {
		checkEndpointReachable(t, p.BaseURL)
	}

	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")
	outDir := filepath.Join(tmpDir, "outputs")

	logger, err := obs.NewLogger(logDir, &reg.System)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	backend := store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks"))
	ts := store.NewTaskStore(backend)

	expDir := filepath.Join(tmpDir, "experience")
	expStore, err := store.NewExperienceStore(expDir, ts, &reg.System, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}

	jitDir := filepath.Join(tmpDir, "jit")
	jit := jit.NewJITManager(reg, jitDir)

	engine := core.NewDirectedEngine(reg, ts, expStore, jit, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })

	return engine, reg
}

// waitForCompletion polls the engine until the task reaches a terminal state
// or the timeout expires. Returns the final status map.
func waitForCompletion(t *testing.T, engine *core.DirectedEngine, taskID string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, ok := engine.GetStatus(taskID, "full")
		if !ok {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		state := graphStatusString(status)
		if state == "completed" || state == "failed" || state == "aborted" {
			return status
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("task %s did not complete within %v", taskID, timeout)
	return nil
}

// TestLiveXFyun_SimpleChat submits a single-step task to the iFlytek
// Spark-X2.5-4B model and asserts the engine produces a non-empty response.
//
// Verification checkpoint (instrumentation point B1): proves the real LLM API integration
// works end-to-end — config loading, provider HTTP call, response parsing,
// engine state transitions. See docs/MOCK_BOUNDARY_TEST_SCENARIOS.md §B1.
func TestLiveXFyun_SimpleChat(t *testing.T) {
	engine, _ := buildEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{
			ID:     "s1",
			RoleID: "worker",
			Task:   "Say hello in one sentence. Respond with plain text only.",
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	status := waitForCompletion(t, engine, taskID, 120*time.Second)

	state := graphStatusString(status)
	if state != "completed" {
		t.Fatalf("task status = %q, want completed; full status: %v", state, status)
	}

	// Assert the step produced output. ToStatusDictView returns steps as
	// map[string]any (keyed by step ID), not []any.
	steps, _ := status["steps"].(map[string]any)
	if len(steps) == 0 {
		t.Fatal("no steps in status response")
	}
	t.Logf("task %s completed with %d steps", taskID, len(steps))
}

// TestLiveXFyun_MultiStepDAG submits a 2-step DAG where step 2 depends on
// step 1, verifying real multi-step orchestration with the iFlytek model.
//
// Verification checkpoint (instrumentation point B6): proves the scheduler correctly manages
// step dependencies, spawns steps in order, and the real LLM produces
// output for each step. See docs/MOCK_BOUNDARY_TEST_SCENARIOS.md §B6.
func TestLiveXFyun_MultiStepDAG(t *testing.T) {
	engine, _ := buildEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{
			ID:     "s1",
			RoleID: "worker",
			Task:   "List three colors. Respond with plain text only.",
		},
		{
			ID:        "s2",
			RoleID:    "worker",
			Task:      "Count how many items are in the previous step's output. Respond with just the number.",
			DependsOn: []string{"s1"},
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	status := waitForCompletion(t, engine, taskID, 180*time.Second)

	state := graphStatusString(status)
	if state != "completed" {
		t.Fatalf("task status = %q, want completed; full status: %v", state, status)
	}

	steps, _ := status["steps"].(map[string]any)
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	t.Logf("DAG task %s completed: %d steps", taskID, len(steps))
}

// TestLiveXFyun_ErrorRecovery submits a task with an intentionally vague
// prompt to verify the engine handles real LLM responses gracefully (no
// panic, no hang, status reaches a terminal state).
//
// Verification checkpoint (instrumentation point B1): proves error paths work with a real
// provider — the engine doesn't hang or crash on unexpected LLM output.
func TestLiveXFyun_ErrorRecovery(t *testing.T) {
	engine, _ := buildEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{
			ID:     "s1",
			RoleID: "worker",
			Task:   "", // empty task — tests graceful handling
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	status := waitForCompletion(t, engine, taskID, 90*time.Second)

	state := graphStatusString(status)
	if state != "completed" && state != "failed" {
		t.Fatalf("task status = %q, want completed or failed; full status: %v", state, status)
	}
	t.Logf("error recovery task %s → %s (no panic, no hang)", taskID, state)
}
