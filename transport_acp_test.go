//go:build !web && !dev && !desktop

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// mockACPEngine implements acpEngine for testing.
type mockACPEngine struct {
	taskID     string
	submitErr  error
	cancelErr  error
	lastPrompt string
	lastRoleID string
	cancelled  string
}

func (m *mockACPEngine) Submit(inputs []schemas.StepInput) (string, error) {
	if len(inputs) > 0 {
		m.lastPrompt = inputs[0].Task
		m.lastRoleID = inputs[0].RoleID
	}
	if m.submitErr != nil {
		return "", m.submitErr
	}
	if m.taskID == "" {
		return "test-task-id", nil
	}
	return m.taskID, nil
}

func (m *mockACPEngine) CancelTask(taskID string) error {
	m.cancelled = taskID
	return m.cancelErr
}

// parseResponses splits newline-delimited JSON from stdout into acpResponse slices.
func parseResponses(t *testing.T, out string) []acpResponse {
	t.Helper()
	var resps []acpResponse
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r acpResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("failed to parse response %q: %v", line, err)
		}
		resps = append(resps, r)
	}
	return resps
}

func TestACP_Initialize(t *testing.T) {
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var stdout bytes.Buffer
	if err := serveACP(&mockACPEngine{}, stdin, &stdout); err != nil {
		t.Fatalf("serveACP error: %v", err)
	}
	resps := parseResponses(t, stdout.String())
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	if resps[0].Error != nil {
		t.Fatalf("unexpected error: %v", resps[0].Error)
	}
	result := resps[0].Result.(map[string]any)
	if result["server"] != "orchestrator" {
		t.Errorf("expected server=orchestrator, got %v", result["server"])
	}
}

func TestACP_Ping(t *testing.T) {
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var stdout bytes.Buffer
	serveACP(&mockACPEngine{}, stdin, &stdout)
	resps := parseResponses(t, stdout.String())
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("expected 1 success response, got %+v", resps)
	}
	result := resps[0].Result.(map[string]any)
	if result["pong"] != true {
		t.Errorf("expected pong=true, got %v", result["pong"])
	}
}

func TestACP_RunTask(t *testing.T) {
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"run_task","params":{"prompt":"refactor auth","role_id":"coder"}}` + "\n")
	var stdout bytes.Buffer
	mock := &mockACPEngine{taskID: "task-42"}
	serveACP(mock, stdin, &stdout)
	resps := parseResponses(t, stdout.String())
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("expected 1 success response, got %+v", resps)
	}
	result := resps[0].Result.(map[string]any)
	if result["task_id"] != "task-42" {
		t.Errorf("expected task_id=task-42, got %v", result["task_id"])
	}
	if mock.lastPrompt != "refactor auth" {
		t.Errorf("engine got prompt %q, want %q", mock.lastPrompt, "refactor auth")
	}
	if mock.lastRoleID != "coder" {
		t.Errorf("engine got role_id %q, want %q", mock.lastRoleID, "coder")
	}
}

func TestACP_Cancel(t *testing.T) {
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"cancel","params":{"task_id":"task-99"}}` + "\n")
	var stdout bytes.Buffer
	mock := &mockACPEngine{}
	serveACP(mock, stdin, &stdout)
	resps := parseResponses(t, stdout.String())
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("expected 1 success response, got %+v", resps)
	}
	if mock.cancelled != "task-99" {
		t.Errorf("engine cancelled %q, want %q", mock.cancelled, "task-99")
	}
}

func TestACP_UnknownMethod(t *testing.T) {
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"frobnicate"}` + "\n")
	var stdout bytes.Buffer
	serveACP(&mockACPEngine{}, stdin, &stdout)
	resps := parseResponses(t, stdout.String())
	if len(resps) != 1 || resps[0].Error == nil {
		t.Fatalf("expected error response, got %+v", resps)
	}
	if resps[0].Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resps[0].Error.Code)
	}
}

func TestACP_MultipleRequests(t *testing.T) {
	stdin := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"run_task","params":{"prompt":"hello"}}` + "\n",
	)
	var stdout bytes.Buffer
	mock := &mockACPEngine{taskID: "multi-1"}
	serveACP(mock, stdin, &stdout)
	resps := parseResponses(t, stdout.String())
	if len(resps) != 3 {
		t.Fatalf("expected 3 responses, got %d", len(resps))
	}
	for i, r := range resps {
		if r.Error != nil {
			t.Errorf("response %d had error: %v", i, r.Error)
		}
	}
	if mock.lastPrompt != "hello" {
		t.Errorf("engine got prompt %q, want %q", mock.lastPrompt, "hello")
	}
}
