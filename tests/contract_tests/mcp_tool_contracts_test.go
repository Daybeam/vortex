package contract_tests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newContractTestApp constructs a minimal App with a real DirectedEngine
// backed by file-based stores. The engine is started and must be stopped
// via the returned cleanup function to avoid Windows temp-dir cleanup races.
func newContractTestApp(t *testing.T) (*tools.App, func()) {
	t.Helper()

	baseDir, err := os.MkdirTemp("", "contract_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	outputBase := filepath.Join(baseDir, "output")
	tmpBase := filepath.Join(baseDir, "tmp")
	expDir := filepath.Join(baseDir, "exp")
	logDir := filepath.Join(baseDir, "logs")
	for _, d := range []string{outputBase, tmpBase, expDir, logDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	// Registry with minimal config
	configPath := filepath.Join(baseDir, "config_test.json")
	reg, err := config.NewRegistry(configPath)
	if err != nil {
		os.RemoveAll(baseDir)
		t.Fatalf("NewRegistry: %v", err)
	}

	// File-based store (db=nil → file mode)
	st, err := store.NewStore(outputBase, expDir, "", &reg.System, nil)
	if err != nil {
		os.RemoveAll(baseDir)
		t.Fatalf("NewStore: %v", err)
	}

	// Logger
	logger, _ := core.NewLogger(logDir, nil)

	// SignalField
	sf := core.NewSignalField(0.01, 1*time.Second)
	sf.Start()

	// DirectedEngine
	engine := core.NewDirectedEngine(reg, st.Tasks, st.Experience, nil, logger, sf, outputBase, tmpBase, nil)

	app := &tools.App{
		Registry:  reg,
		Scheduler: engine,
		TaskStore: st.Tasks,
		ExpStore:  st.Experience,
		Logger:    logger,
		Tier:      tools.TierAdmin,
	}

	cleanup := func() {
		engine.Stop()
		sf.Stop()
		logger.Close()
		os.RemoveAll(baseDir)
	}
	return app, cleanup
}

// registerTestServer creates a test MCP server with all tools registered.
func registerTestServer(app *tools.App) *server.MCPServer {
	s := server.NewMCPServer("contract-test", "0.0.0")
	tools.RegisterAll(s, app)
	return s
}

// callTool invokes a registered tool by name and returns its result.
func callTool(t *testing.T, s *server.MCPServer, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	st := s.GetTool(name)
	if st == nil {
		t.Fatalf("tool %q not registered", name)
	}
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
	return st.Handler(context.Background(), req)
}

// parseResultJSON extracts the JSON content from a tool result text.
func parseResultJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatal("nil result")
	}
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("unmarshal result: %v (text: %s)", err, text.Text)
	}
	return out
}

// ─── Contract: Tool Registration ─────────────────────────────────────────

func TestMCPContract_AllToolsRegistered(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	expected := []string{
		"orchestrator_submit_task",
		"orchestrator_get_task_status",
		"orchestrator_wait_task",
		"orchestrator_discover",
	}
	for _, name := range expected {
		if s.GetTool(name) == nil {
			t.Errorf("contract violation: tool %q not registered", name)
		}
	}
}

// ─── Contract: submit_task Error Paths ───────────────────────────────────
// These test the input validation contract: invalid input must produce
// an error result, not a panic or malformed success response.

func TestMCPContract_SubmitTask_MissingStepsAndTask_Error(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	res, err := callTool(t, s, "orchestrator_submit_task", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !res.IsError {
		t.Errorf("contract violation: expected error result for missing steps+task, got success")
	}
}

func TestMCPContract_SubmitTask_EmptySteps_Error(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	res, err := callTool(t, s, "orchestrator_submit_task", map[string]any{
		"steps": []map[string]any{},
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !res.IsError {
		t.Errorf("contract violation: expected error for empty steps array, got success")
	}
}

func TestMCPContract_SubmitTask_StepsWithNoTask_Error(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	res, err := callTool(t, s, "orchestrator_submit_task", map[string]any{
		"steps": []map[string]any{
			{"id": "s1", "role_id": "coder"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !res.IsError {
		t.Errorf("contract violation: expected error for step with no task field, got success")
	}
}

// ─── Contract: submit_task Success Path ──────────────────────────────────
// This tests the output schema contract: valid input must produce output
// with the required fields (task_id, complexity_hint, step_count).
// If the output schema changes, this test breaks — that's the point.

func TestMCPContract_SubmitTask_ValidTask_OutputSchema(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	res, err := callTool(t, s, "orchestrator_submit_task", map[string]any{
		"task": "Write a hello world in Go",
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("contract violation: expected success for valid task, got error")
	}

	out := parseResultJSON(t, res)

	// Required field: task_id (string)
	taskID, ok := out["task_id"].(string)
	if !ok || taskID == "" {
		t.Errorf("contract violation: task_id must be a non-empty string, got %T %v", out["task_id"], out["task_id"])
	}

	// Required field: complexity_hint (string)
	hint, ok := out["complexity_hint"].(string)
	if !ok || hint == "" {
		t.Errorf("contract violation: complexity_hint must be a non-empty string, got %T %v", out["complexity_hint"], out["complexity_hint"])
	}

	// Required field: step_count (number)
	stepCount, ok := out["step_count"].(float64)
	if !ok || stepCount < 1 {
		t.Errorf("contract violation: step_count must be a number >= 1, got %T %v", out["step_count"], out["step_count"])
	}

	// Required field: auto_matched (must be present, nil or object)
	if _, ok := out["auto_matched"]; !ok {
		t.Errorf("contract violation: auto_matched field missing from output")
	}

	// Required field: adaptive_verifier (must be present, nil or array)
	if _, ok := out["adaptive_verifier"]; !ok {
		t.Errorf("contract violation: adaptive_verifier field missing from output")
	}
}

// ─── Contract: get_task_status ───────────────────────────────────────────

func TestMCPContract_GetTaskStatus_NonExistent_Error(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	res, err := callTool(t, s, "orchestrator_get_task_status", map[string]any{
		"task_id": "nonexistent_task_12345",
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !res.IsError {
		t.Errorf("contract violation: expected error for non-existent task_id, got success")
	}
}

func TestMCPContract_GetTaskStatus_ExistingTask_ReturnsStatus(t *testing.T) {
	app, cleanup := newContractTestApp(t)
	defer cleanup()
	s := registerTestServer(app)

	// First submit a task
	submitRes, err := callTool(t, s, "orchestrator_submit_task", map[string]any{
		"task": "Test task for status contract",
	})
	if err != nil || submitRes.IsError {
		t.Fatalf("submit failed: err=%v, isError=%v", err, submitRes.IsError)
	}
	submitOut := parseResultJSON(t, submitRes)
	taskID := submitOut["task_id"].(string)

	// Then query its status
	statusRes, err := callTool(t, s, "orchestrator_get_task_status", map[string]any{
		"task_id": taskID,
	})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if statusRes.IsError {
		t.Fatalf("contract violation: expected success for existing task, got error")
	}

	// The status result must be valid JSON (parseResultJSON asserts this)
	_ = parseResultJSON(t, statusRes)
}
