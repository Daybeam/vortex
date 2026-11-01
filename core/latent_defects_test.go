package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/jit"
)

// Sieve-only coverage (history, threshold, loop detection) moved with the
// package to core/sieve/sieve_history_test.go.

func TestJIT_RestoreLocalMCPs_SkipsMissingScript(t *testing.T) {
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := jit.NewJITManager(reg, tempDir)

	id := "jit_test_missing"
	missingPath := filepath.Join(tempDir, "nonexistent.lua")
	mcpDef := config.MCPDef{
		ID:      id,
		Command: "lua",
		Args:    []string{missingPath},
	}

	reg.Mu.Lock()
	reg.DynamicMCPs[id] = &mcpDef
	reg.Mu.Unlock()

	snap := jit.SnapshotJITTools([]string{id})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	reg.Mu.Lock()
	delete(reg.DynamicMCPs, id)
	reg.Mu.Unlock()

	restored := jit.RestoreLocalMCPs(snap)
	if len(restored) != 0 {
		t.Fatalf("should not restore tool with missing script, got %d restored", len(restored))
	}
}

func TestJIT_RestoreLocalMCPs_RestoresWhenScriptExists(t *testing.T) {
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := jit.NewJITManager(reg, tempDir)

	id := "jit_test_exists"
	scriptPath := filepath.Join(tempDir, "test.lua")
	if err := os.WriteFile(scriptPath, []byte("print('hello')"), 0644); err != nil {
		t.Fatal(err)
	}

	mcpDef := config.MCPDef{
		ID:      id,
		Command: "lua",
		Args:    []string{scriptPath},
	}

	reg.Mu.Lock()
	reg.DynamicMCPs[id] = &mcpDef
	reg.Mu.Unlock()

	snap := jit.SnapshotJITTools([]string{id})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	reg.Mu.Lock()
	delete(reg.DynamicMCPs, id)
	reg.Mu.Unlock()

	restored := jit.RestoreLocalMCPs(snap)
	if len(restored) != 1 {
		t.Fatalf("should restore tool with existing script, got %d restored", len(restored))
	}
}

func TestJIT_RestoreLocalMCPs_EmptyArgs_RestoresWithoutCheck(t *testing.T) {
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := jit.NewJITManager(reg, tempDir)

	id := "jit_no_args"
	mcpDef := config.MCPDef{
		ID:      id,
		Command: "custom",
	}

	reg.Mu.Lock()
	reg.DynamicMCPs[id] = &mcpDef
	reg.Mu.Unlock()

	snap := jit.SnapshotJITTools([]string{id})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	reg.Mu.Lock()
	delete(reg.DynamicMCPs, id)
	reg.Mu.Unlock()

	restored := jit.RestoreLocalMCPs(snap)
	if len(restored) != 1 {
		t.Fatalf("tool with no Args should be restored without file check, got %d", len(restored))
	}
}

func TestToolRouter_PrivilegedToolsBlocked(t *testing.T) {
	tools := []string{
		"orchestrator_admin_deploy",
		"orchestrator_cancel_task",
		"orchestrator_fork_task",
		"orchestrator_run_command",
		"orchestrator_invoke",
		"orchestrator_debug_dump",
		"orchestrator_reload",
		"orchestrator_core_replace_batch",
		"orchestrator_submit_decision",
	}

	for _, tool := range tools {
		if !privilegedToolBlocklist[tool] {
			t.Errorf("tool %s should be in privilegedToolBlocklist", tool)
		}
	}
}
