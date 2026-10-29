package core

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ── Mock ACP Server (re-exec pattern) ───────────────────────────────────────
//
// When ACP_MOCK_SERVER=1 is set, the test binary re-execs as a mock ACP
// server. The ACP_MOCK_BEHAVIOR env var selects the scenario:
//   "ok"          — initialize ok, run_task → task_completed (default)
//   "init_error"  — initialize returns error
//   "task_failed" — run_task → task_failed
//   "streaming"   — run_task → progress × 2 → task_completed
//   "hang"        — initialize ok, run_task responds but never sends completion

func acpMockServerMain() {
	behavior := os.Getenv("ACP_MOCK_BEHAVIOR")
	if behavior == "" {
		behavior = "ok"
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)

	writeJSON := func(v any) {
		data, _ := json.Marshal(v)
		os.Stdout.Write(append(data, '\n'))
	}
	writeResp := func(id json.RawMessage, result any) {
		writeJSON(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  any             `json:"result"`
		}{"2.0", id, result})
	}
	writeErr := func(id json.RawMessage, code int, msg string) {
		writeJSON(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   map[string]any  `json:"error"`
		}{"2.0", id, map[string]any{"code": code, "message": msg}})
	}
	writeNotif := func(method string, params any) {
		writeJSON(struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  any    `json:"params"`
		}{"2.0", method, params})
	}

	for scanner.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			if behavior == "init_error" {
				writeErr(req.ID, -32000, "mock init error")
			} else {
				writeResp(req.ID, map[string]any{"server": "mock", "version": "1.0"})
			}
		case "run_task":
			writeResp(req.ID, map[string]any{"task_id": "mock-task-1"})
			switch behavior {
			case "task_failed":
				writeNotif("task_failed", map[string]any{
					"task_id": "mock-task-1",
					"error":   "mock task failure",
				})
			case "streaming":
				writeNotif("progress", map[string]any{"message": "step 1 done"})
				writeNotif("progress", map[string]any{"message": "step 2 done"})
				writeNotif("task_completed", map[string]any{
					"task_id": "mock-task-1",
					"report":  "streaming task completed",
				})
			case "hang":
				// never send completion — test will use context timeout
				time.Sleep(3 * time.Second)
			case "flood":
				// R-28-1 regression: flood 200 notifications without
				// completion. The client's short context expires long
				// before all 200 are consumed, so after ExecuteTask
				// returns (stops consuming), readLoop drains the pipe
				// buffer, fills msgCh (cap 32), and blocks on the 33rd
				// send. Then block on stdin so we stay alive (stdout
				// open) until Close() closes stdin.
				for i := 0; i < 200; i++ {
					writeNotif("progress", map[string]any{"n": i})
				}
				scanner.Scan() // block until stdin closes
			default:
				writeNotif("task_completed", map[string]any{
					"task_id": "mock-task-1",
					"report":  "mock task completed successfully",
				})
			}
		default:
			writeErr(req.ID, -32601, "method not found")
		}
	}
}

// startMockACP starts the mock ACP server with the given behavior and returns
// an ACPClient connected to it. The caller must Close() the client.
func startMockACP(t *testing.T, behavior string) *ACPClient {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		"ACP_MOCK_SERVER=1",
		"ACP_MOCK_BEHAVIOR="+behavior,
	)
	client, err := acpClientFromCmd(cmd)
	if err != nil {
		t.Fatalf("start mock ACP server: %v", err)
	}
	return client
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestACPClient_InitializeAndExecuteTask(t *testing.T) {
	client := startMockACP(t, "ok")
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	result, err := client.ExecuteTask(ctx, "refactor the auth module", "coder", nil)
	if err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}

	report, ok := result["report"].(string)
	if !ok || !strings.Contains(report, "completed") {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestACPClient_InitializeError(t *testing.T) {
	client := startMockACP(t, "init_error")
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Initialize(ctx)
	if err == nil {
		t.Fatal("expected error from Initialize, got nil")
	}
	if !strings.Contains(err.Error(), "mock init error") {
		t.Fatalf("expected 'mock init error' in message, got: %v", err)
	}
}

func TestACPClient_TaskFailed(t *testing.T) {
	client := startMockACP(t, "task_failed")
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := client.ExecuteTask(ctx, "do something", "coder", nil)
	if err == nil {
		t.Fatal("expected error from ExecuteTask, got nil")
	}
	if !strings.Contains(err.Error(), "mock task failure") {
		t.Fatalf("expected 'mock task failure' in message, got: %v", err)
	}
}

func TestACPClient_EventStreaming(t *testing.T) {
	client := startMockACP(t, "streaming")
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	var events []string
	result, err := client.ExecuteTask(ctx, "multi-step task", "coder", func(method string, _ map[string]any) {
		events = append(events, method)
	})
	if err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 progress events, got %d: %v", len(events), events)
	}
	if events[0] != "progress" || events[1] != "progress" {
		t.Fatalf("expected [progress progress], got %v", events)
	}

	report, _ := result["report"].(string)
	if !strings.Contains(report, "streaming") {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestACPClient_ContextCancel(t *testing.T) {
	client := startMockACP(t, "hang")
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := client.ExecuteTask(ctx, "hanging task", "coder", nil)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
}

// TestACPClient_CloseDuringFlood is the regression test for audit R-28-1:
// Close() must not deadlock when readLoop is blocked on a full msgCh.
//
// The mock "flood" behavior writes 40 notifications (>msgCh cap 32) without
// completion, then blocks on stdin. After the caller's context times out and
// ExecuteTask returns (stops consuming), readLoop fills msgCh and blocks on
// the 33rd channel send. Before the fix, Close() closed stdin and killed the
// child, but readLoop was blocked on the send (not scanner.Scan), so <-c.done
// hung forever. After the fix, Close() closes a quit channel that readLoop
// selects on, unblocking it regardless of channel state.
func TestACPClient_CloseDuringFlood(t *testing.T) {
	client := startMockACP(t, "flood")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Send run_task directly (not ExecuteTask). The mock responds with the
	// task_id, then floods 200 notifications. sendRequest consumes only the
	// task_id response (matched by ID) and returns — the 200 notifications
	// remain in the pipe for readLoop to read with NO consumer draining
	// msgCh. readLoop fills msgCh (cap 32) and blocks on the 33rd send.
	_, err := client.sendRequest(ctx, "run_task", acpRunTaskParams{Prompt: "flood", RoleID: "coder"}, nil)
	if err != nil {
		t.Fatalf("sendRequest run_task: %v", err)
	}

	// Give readLoop time to drain the pipe buffer into msgCh and block.
	time.Sleep(100 * time.Millisecond)

	// Close() must return promptly. Before the fix it deadlocked at <-c.done
	// because readLoop was blocked on the channel send (not scanner.Scan).
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-done:
		// success — Close() returned without deadlocking
	case <-time.After(15 * time.Second):
		t.Fatal("R-28-1: Close() deadlocked — readLoop blocked on full msgCh and quit was not signaled")
	}
}
