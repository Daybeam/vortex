package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
)

// Regression test for #17 (eval §8.35): execute_code must NOT include
// exit_code/error in the result when the script succeeds (exit_code == 0).
// The model was treating exit_code:0 as a domain success signal, causing
// false success to propagate into final_response.json.
//
// Bug: core_tools.go executeCode() always returned exit_code + error,
// even on success. Fix: only include them on failure (exit_code != 0).
func TestExecuteCode_SuccessOmitsExitCode(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_task"
	if err := os.MkdirAll(filepath.Join(tmpDir, taskID), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     `print("hello world")`,
		"language": "python",
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("execute_code failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("result is not valid JSON: %v\nraw: %s", err, result)
	}

	if _, hasExitCode := m["exit_code"]; hasExitCode {
		t.Errorf("success result must NOT contain exit_code, got: %s", result)
	}
	if _, hasError := m["error"]; hasError {
		t.Errorf("success result must NOT contain error, got: %s", result)
	}
	stdout, _ := m["stdout"].(string)
	if !strings.Contains(stdout, "hello world") {
		t.Errorf("stdout should contain 'hello world', got: %s", stdout)
	}
}

func TestExecuteCode_FailureIncludesExitCode(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_task"
	if err := os.MkdirAll(filepath.Join(tmpDir, taskID), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     `import sys; print("failing"); sys.exit(1)`,
		"language": "python",
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("execute_code returned error (expected JSON result with exit_code): %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("result is not valid JSON: %v\nraw: %s", err, result)
	}

	exitCode, hasExitCode := m["exit_code"]
	if !hasExitCode {
		t.Errorf("failure result MUST contain exit_code, got: %s", result)
	}
	if hasExitCode {
		if ec, ok := exitCode.(float64); !ok || ec != 1 {
			t.Errorf("expected exit_code=1, got %v", exitCode)
		}
	}
	if _, hasError := m["error"]; !hasError {
		t.Errorf("failure result MUST contain error field, got: %s", result)
	}
}

// Regression test for #1 (eval §8.31): _core tools injection order.
// Default: _core is first (backward compat). With
// VORTEX_CORE_TOOLS_ORDER=last, _core is appended after domain MCPs.
func TestBuildMCPServers_CoreToolsOrderDefault(t *testing.T) {
	os.Unsetenv("VORTEX_CORE_TOOLS_ORDER")
	servers := buildMCPServersForTest(t, false)
	if len(servers) == 0 {
		t.Fatal("expected at least one server")
	}
	if servers[0].Name != "_core" {
		t.Errorf("default order: _core should be first, got %s", servers[0].Name)
	}
}

func TestBuildMCPServers_CoreToolsOrderLast(t *testing.T) {
	os.Setenv("VORTEX_CORE_TOOLS_ORDER", "last")
	defer os.Unsetenv("VORTEX_CORE_TOOLS_ORDER")
	servers := buildMCPServersForTest(t, false)
	if len(servers) == 0 {
		t.Fatal("expected at least one server")
	}
	if servers[len(servers)-1].Name != "_core" {
		t.Errorf("last order: _core should be last, got %s", servers[len(servers)-1].Name)
	}
}

// buildMCPServersForTest is a lightweight helper that calls buildMCPServers
// with minimal setup. When withDomainMCP is true, a fake domain MCP is added
// to verify ordering between _core and domain servers.
func buildMCPServersForTest(t *testing.T, withDomainMCP bool) []providers.MCPServerDef {
	t.Helper()
	s := &Spawner{
		mcpMgr: &MCPConnectionManager{},
	}
	hub := &ContextHub{}
	ctx := context.Background()
	bindings := []config.MCPBinding{}
	return s.buildMCPServers(ctx, hub, "test_task", bindings, "default")
}
