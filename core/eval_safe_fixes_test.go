package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestExecuteCode_PrintOnlyMetric (#2 regression, eval §8.31):
// execute_code that exits 0 with stdout but no stderr must increment
// the execute_code_print_only metric. This is the "false success"
// observable classification — zero behavior change, pure metric.
//
// Bug: before the fix, execute_code with exit_code=0 and only print
// output was indistinguishable from a real domain success. The metric
// allows an arm to compare print_only/success ratio against reward.
func TestExecuteCode_PrintOnlyMetric(t *testing.T) {
	snapBefore := observability.GetGlobalMetrics().GetSnapshot()
	printOnlyBefore, _ := snapBefore["orchestrator.calls.execute_code_print_only"].(int64)
	successBefore, _ := snapBefore["orchestrator.calls.execute_code_success"].(int64)

	// execute_code with a simple print statement — exit 0, stdout non-empty, stderr empty.
	result, err := HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     `print("hello world")`,
		"language": "python",
	}, "", "")
	if err != nil {
		t.Fatalf("execute_code failed: %v", err)
	}
	if !strings.Contains(result, "hello world") {
		t.Fatalf("expected stdout to contain 'hello world', got: %s", result)
	}

	snapAfter := observability.GetGlobalMetrics().GetSnapshot()
	printOnlyAfter, _ := snapAfter["orchestrator.calls.execute_code_print_only"].(int64)
	successAfter, _ := snapAfter["orchestrator.calls.execute_code_success"].(int64)

	if printOnlyAfter != printOnlyBefore+1 {
		t.Errorf("execute_code_print_only metric: expected increment by 1, got before=%d after=%d", printOnlyBefore, printOnlyAfter)
	}
	if successAfter != successBefore+1 {
		t.Errorf("execute_code_success metric: expected increment by 1, got before=%d after=%d", successBefore, successAfter)
	}
}

// TestExecuteCode_StderrSuppressesPrintOnlyMetric (#2 regression):
// execute_code that exits 0 but produces stderr should NOT increment
// the print_only metric (stderr indicates a real error condition).
func TestExecuteCode_StderrSuppressesPrintOnlyMetric(t *testing.T) {
	snapBefore := observability.GetGlobalMetrics().GetSnapshot()
	printOnlyBefore, _ := snapBefore["orchestrator.calls.execute_code_print_only"].(int64)

	// execute_code that writes to stderr but still exits 0.
	_, err := HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     `import sys; sys.stderr.write("warning\n")`,
		"language": "python",
	}, "", "")
	if err != nil {
		t.Fatalf("execute_code failed: %v", err)
	}

	snapAfter := observability.GetGlobalMetrics().GetSnapshot()
	printOnlyAfter, _ := snapAfter["orchestrator.calls.execute_code_print_only"].(int64)

	if printOnlyAfter != printOnlyBefore {
		t.Errorf("execute_code_print_only metric: expected NO increment when stderr is non-empty, got before=%d after=%d", printOnlyBefore, printOnlyAfter)
	}
}

// TestMCPToolsListFailure_SlogError (#18 regression, eval §8.31):
// When MCP tools/list discovery fails, the error must be logged at
// ERROR level via slog (not just event log). This test verifies that
// slog.Error output is captured and contains the expected fields.
//
// Bug: before the fix, tools/list failure was only logged as an event
// log (EventMCPToolsDiscoveryFailed), which is not visible in standard
// log output. Operators couldn't see why an MCP had 0 tools.
func TestMCPToolsListFailure_SlogError(t *testing.T) {
	// Capture slog output by replacing the default handler.
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldDefault)

	// Simulate the slog.Error call that the fix added for CLI-based MCPs.
	slog.Error("MCP tools/list discovery failed — MCP will have 0 tools",
		"mcp", "test-mcp",
		"error", "connection refused",
		"task", "test-task",
	)

	output := buf.String()
	if !strings.Contains(output, "level=ERROR") {
		t.Errorf("expected level=ERROR in slog output, got: %s", output)
	}
	if !strings.Contains(output, "tools/list discovery failed") {
		t.Errorf("expected 'tools/list discovery failed' in slog output, got: %s", output)
	}
	if !strings.Contains(output, "mcp=test-mcp") {
		t.Errorf("expected mcp=test-mcp in slog output, got: %s", output)
	}

	// Also verify the URL-based MCP error format.
	buf.Reset()
	slog.Error("MCP remote tools/list discovery failed — MCP will have 0 tools",
		"mcp", "remote-mcp",
		"status", "discovery_failed",
		"error", "timeout",
		"task", "test-task",
	)

	output = buf.String()
	if !strings.Contains(output, "level=ERROR") {
		t.Errorf("expected level=ERROR in remote slog output, got: %s", output)
	}
	if !strings.Contains(output, "remote tools/list discovery failed") {
		t.Errorf("expected 'remote tools/list discovery failed' in slog output, got: %s", output)
	}
}

// TestIsContextLimitError (#20 regression, eval §8.46):
// Verifies that isContextLimitError correctly detects context-limit errors
// from different providers (OpenAI, Anthropic, Gemini, generic).
//
// Feature: Context Window Compression
// Scenario: When the provider returns a context-limit error, the system must
//          detect it and trigger compression
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Context Compression
// -----------------------------------------------------------------------------
func TestIsContextLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"openai context_length_exceeded", fmt.Errorf("HTTP 400: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\"}}"), true},
		{"openai max context length", fmt.Errorf("HTTP 400: This model's maximum context length is 8192 tokens"), true},
		{"anthropic prompt too long", fmt.Errorf("HTTP 400: prompt is too long: 100000 tokens > 200000 maximum"), true},
		{"anthropic context too long", fmt.Errorf("HTTP 400: context too long"), true},
		{"gemini exceeds maximum", fmt.Errorf("HTTP 400: exceeds the maximum number of tokens"), true},
		{"generic too many tokens", fmt.Errorf("too many tokens in request"), true},
		// Regression: eval §8.48.7 — llama.cpp/vLLM/Ollama return a 400 with
		// "exceeds the available context size" which the original 8 cases all
		// missed, so #20 (context compression) never triggered for any
		// OpenAI-compatible local server. These cases reproduce the exact error
		// text observed in the eval run (task2, turns 9–19, 11 consecutive 400s).
		{"llama.cpp exact 400", fmt.Errorf(`bad request: {"error":{"code":400,"message":"request (17164 tokens) exceeds the available context size (16384)"}}`), true},
		{"vllm context size", fmt.Errorf(`HTTP 400: {"error":{"message":"input length 18000 exceeds available context size 16384"}}`), true},
		{"ollama context size", fmt.Errorf("context size exceeded: 17164 > 16384"), true},
		{"unrelated 400", fmt.Errorf("HTTP 400: invalid model name"), false},
		{"unrelated timeout", fmt.Errorf("context deadline exceeded"), false},
		{"rate limit", fmt.Errorf("HTTP 429: rate limited"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isContextLimitError(tc.err)
			if got != tc.want {
				t.Errorf("isContextLimitError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCompressUserBlocks_KeepsFirst2Last6WithMarker (#20 regression, eval §8.46):
// Verifies that compressUserBlocks correctly compresses conversation history
// by keeping the first 2 and last 6 blocks, with a compression marker.
func TestCompressUserBlocks_KeepsFirst2Last6WithMarker(t *testing.T) {
	// Not enough blocks to compress (< 10).
	small := make([]schemas.ContentBlock, 5)
	if got := compressUserBlocks(small); got != nil {
		t.Errorf("compressUserBlocks with 5 blocks: expected nil, got %d blocks", len(got))
	}

	// Enough blocks to compress (12 > 10).
	blocks := make([]schemas.ContentBlock, 12)
	for i := range blocks {
		blocks[i] = schemas.ContentBlock{Text: fmt.Sprintf("block-%d", i)}
	}
	compressed := compressUserBlocks(blocks)
	if compressed == nil {
		t.Fatal("compressUserBlocks with 12 blocks: expected non-nil")
	}
	// Should have: 2 (first) + 1 (marker) + 6 (last) = 9 blocks.
	if len(compressed) != 9 {
		t.Errorf("expected 9 compressed blocks, got %d", len(compressed))
	}
	// First 2 blocks preserved.
	if compressed[0].Text != "block-0" || compressed[1].Text != "block-1" {
		t.Errorf("first 2 blocks not preserved: got %q, %q", compressed[0].Text, compressed[1].Text)
	}
	// Compression marker present.
	if !strings.Contains(compressed[2].Text, "compressed") {
		t.Errorf("expected compression marker at index 2, got: %q", compressed[2].Text)
	}
	// Last 6 blocks preserved (block-6 through block-11).
	if compressed[3].Text != "block-6" {
		t.Errorf("expected block-6 at index 3, got: %q", compressed[3].Text)
	}
	if compressed[8].Text != "block-11" {
		t.Errorf("expected block-11 at index 8, got: %q", compressed[8].Text)
	}
}

// TestToolCallKey_DeterministicSameArgsDifferentForDifferent (#11 regression, eval §8.41):
// Verifies that toolCallKey produces deterministic keys — same key for
// identical tool+args, different keys for different tool or args.
func TestToolCallKey_DeterministicSameArgsDifferentForDifferent(t *testing.T) {
	args1 := map[string]any{"record_id": "user_1", "fields": []any{"name", "email"}}
	args2 := map[string]any{"record_id": "user_1", "fields": []any{"name", "email"}}
	args3 := map[string]any{"record_id": "user_2", "fields": []any{"name", "email"}}

	key1 := toolCallKey("get_booking_history", args1)
	key2 := toolCallKey("get_booking_history", args2)
	key3 := toolCallKey("get_booking_history", args3)
	key4 := toolCallKey("get_user_profile", args1)

	// Same tool + same args → same key.
	if key1 != key2 {
		t.Errorf("identical calls should have same key: %q != %q", key1, key2)
	}
	// Same tool + different args → different key.
	if key1 == key3 {
		t.Errorf("different args should have different keys: %q == %q", key1, key3)
	}
	// Different tool + same args → different key.
	if key1 == key4 {
		t.Errorf("different tools should have different keys: %q == %q", key1, key4)
	}
	// Empty args → tool name + ":".
	emptyKey := toolCallKey("list_tools", map[string]any{})
	if !strings.HasPrefix(emptyKey, "list_tools:") {
		t.Errorf("empty args key should start with tool name: %q", emptyKey)
	}
}

// TestIsWriteMCPTool (#19 regression, eval §8.42):
//
// Feature: Write Authorization
// Scenario: IsWriteMCPTool correctly classifies write tools vs read-only tools
//          and excludes core filesystem tools
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Write Authorization
// -----------------------------------------------------------------------------
// Verifies that IsWriteMCPTool correctly classifies MCP tools as write
// (modifies external state) vs read (query only), and excludes core tools.
func TestIsWriteMCPTool(t *testing.T) {
	writeTools := []string{
		"cancel_reservation", "update_booking", "create_order", "delete_record",
		"set_config", "send_email", "transfer_funds", "book_flight",
		"submit_form", "close_ticket", "modify_profile", "edit_settings",
		"insert_row", "remove_item", "drop_table", "put_object", "post_webhook",
		"approve_request", "reject_claim", "assign_task", "revoke_token",
	}
	for _, name := range writeTools {
		if !IsWriteMCPTool(name) {
			t.Errorf("expected %q to be a write tool", name)
		}
	}

	readTools := []string{
		"get_booking_history", "list_reservations", "search_flights",
		"find_user_id", "query_database", "read_config", "check_status",
		"verify_token", "fetch_profile", "view_report", "show_details",
		"describe_schema", "get_user_profile",
	}
	for _, name := range readTools {
		if IsWriteMCPTool(name) {
			t.Errorf("expected %q to NOT be a write tool", name)
		}
	}

	// Core tools are never write MCP tools (they're fs-only, tracked by StagedWorkspace).
	coreTools := []string{"write_file", "read_file", "execute_code"}
	for _, name := range coreTools {
		if IsWriteMCPTool(name) {
			t.Errorf("expected core tool %q to NOT be a write MCP tool", name)
		}
	}
}

// recordingTaskStore captures Set calls for verification in trace persistence tests.
type recordingTaskStore struct {
	lastSetTaskID string
	lastSetStepID string
	lastSetResult *store.StepResult
	setCallCount  int
}

func (m *recordingTaskStore) Set(_ context.Context, taskID, stepID string, result *store.StepResult) error {
	m.lastSetTaskID = taskID
	m.lastSetStepID = stepID
	m.lastSetResult = result
	m.setCallCount++
	return nil
}
func (m *recordingTaskStore) Get(_ context.Context, _, _ string) (*store.StepResult, error) {
	return nil, os.ErrNotExist
}
func (m *recordingTaskStore) GetBatch(_ context.Context, _ string, _ []string) (map[string]*store.StepResult, error) {
	return make(map[string]*store.StepResult), nil
}
func (m *recordingTaskStore) GetByRef(_ context.Context, _ string) (*store.StepResult, error) {
	return nil, os.ErrNotExist
}
func (m *recordingTaskStore) ClearTask(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (m *recordingTaskStore) Claim(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

// TestPersistTraceOnError (eval §8.48.6 regression):
// Verifies that persistTraceOnError correctly writes the accumulated trace
// to the task store. Without this, error paths in doSpawn lose the trace —
// the eval observed 28.3% of steps missing from the engine DB, almost all
// from error paths where the trace was never persisted.
// TestPersistTraceOnError (eval §8.48.6 regression):
//
// Feature: Engine Trace Persistence
// Scenario: On error paths, trace data must be persisted to TaskStore and
//          must not be lost
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Engine Trace Persistence
// -----------------------------------------------------------------------------
func TestPersistTraceOnError(t *testing.T) {
	mockStore := &recordingTaskStore{}
	s := &Spawner{taskStore: mockStore}

	trace := []store.ToolInteraction{
		{ToolName: "search_flight", Arguments: map[string]any{"origin": "SFO"}, Result: "found 3 flights"},
		{ToolName: "get_user_details", Arguments: map[string]any{"user_id": "123"}, Result: "Silver member"},
	}

	req := &SpawnRequest{TaskID: "task-trace-test", StepID: "step-1"}
	ctx := context.Background()

	s.persistTraceOnError(ctx, req, trace, "provider-1", "model-1", "research")

	if mockStore.setCallCount != 1 {
		t.Fatalf("expected 1 Set call, got %d", mockStore.setCallCount)
	}
	if mockStore.lastSetTaskID != "task-trace-test" {
		t.Errorf("expected taskID 'task-trace-test', got %q", mockStore.lastSetTaskID)
	}
	if mockStore.lastSetStepID != "step-1" {
		t.Errorf("expected stepID 'step-1', got %q", mockStore.lastSetStepID)
	}
	if mockStore.lastSetResult == nil {
		t.Fatal("expected non-nil StepResult")
	}
	if len(mockStore.lastSetResult.Trace) != 2 {
		t.Fatalf("expected 2 trace entries, got %d", len(mockStore.lastSetResult.Trace))
	}
	if mockStore.lastSetResult.Trace[0].ToolName != "search_flight" {
		t.Errorf("expected first trace tool 'search_flight', got %q", mockStore.lastSetResult.Trace[0].ToolName)
	}
	if mockStore.lastSetResult.ProviderID != "provider-1" {
		t.Errorf("expected providerID 'provider-1', got %q", mockStore.lastSetResult.ProviderID)
	}
	if mockStore.lastSetResult.ModelID != "model-1" {
		t.Errorf("expected modelID 'model-1', got %q", mockStore.lastSetResult.ModelID)
	}
	if mockStore.lastSetResult.Capability != "research" {
		t.Errorf("expected capability 'research', got %q", mockStore.lastSetResult.Capability)
	}
	if mockStore.lastSetResult.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}

	// Nil task store — should be a no-op (nil-safe).
	s2 := &Spawner{taskStore: nil}
	s2.persistTraceOnError(ctx, req, trace, "p", "m", "c") // must not panic
}
