package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func TestSieve_ClearHistory_PreventsMemoryLeak(t *testing.T) {
	s := NewSieve(10)
	taskID := "task-leak-test"

	for i := 0; i < 5; i++ {
		call := store.ToolInteraction{
			ToolName:  "read_file",
			Arguments: map[string]any{"path": filepath.Join("/tmp", string(rune('a'+i)))},
		}
		valid, _ := s.InspectToolCalls(taskID, []store.ToolInteraction{call})
		if !valid {
			t.Fatalf("call %d should be valid", i)
		}
	}

	s.ClearHistory(taskID)

	s.mu.Lock()
	_, exists := s.toolHistory[taskID]
	s.mu.Unlock()
	if exists {
		t.Fatal("ClearHistory should remove task from toolHistory map")
	}
}

func TestSieve_AlternatingFailure_DetectedAsLoop(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3
	taskID := "task-alt-test"

	callA := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "/tmp/a"}}
	callB := store.ToolInteraction{ToolName: "read_file", Arguments: map[string]any{"path": "/tmp/a"}}

	for i := 0; i < 4; i++ {
		s.InspectToolCalls(taskID, []store.ToolInteraction{callA})
		s.InspectToolCalls(taskID, []store.ToolInteraction{callB})
	}

	valid, reason := s.InspectToolCalls(taskID, []store.ToolInteraction{callA})
	if valid {
		t.Fatalf("alternating pattern should be detected as potential loop, got valid=true")
	}
	if reason != "InfiniteLoopDetected" {
		t.Fatalf("expected InfiniteLoopDetected, got %s", reason)
	}
}

func TestSieve_ConfigurableThreshold(t *testing.T) {
	sys := config.SystemSettings{
		MaxContextKeep:          100,
		ToolRepetitionThreshold: 5,
	}
	s := newSieveFromConfig(sys)
	if s.ToolRepetitionThreshold != 5 {
		t.Fatalf("expected threshold 5 from config, got %d", s.ToolRepetitionThreshold)
	}

	sys2 := config.SystemSettings{MaxContextKeep: 100}
	s2 := newSieveFromConfig(sys2)
	if s2.ToolRepetitionThreshold != 3 {
		t.Fatalf("expected default threshold 3, got %d", s2.ToolRepetitionThreshold)
	}
}

func TestJIT_RestoreLocalMCPs_SkipsMissingScript(t *testing.T) {
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

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
	jit := NewJITManager(reg, tempDir)

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
	jit := NewJITManager(reg, tempDir)

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

func TestSieve_ClearHistory_OnTaskCompletion(t *testing.T) {
	s := NewSieve(10)
	taskID := "task-completion-test"

	call := store.ToolInteraction{ToolName: "read_file", Arguments: map[string]any{"path": "/tmp/a"}}
	s.InspectToolCalls(taskID, []store.ToolInteraction{call})

	s.mu.Lock()
	_, existsBefore := s.toolHistory[taskID]
	s.mu.Unlock()
	if !existsBefore {
		t.Fatal("task should exist in history before ClearHistory")
	}

	s.ClearHistory(taskID)

	s.mu.Lock()
	_, existsAfter := s.toolHistory[taskID]
	s.mu.Unlock()
	if existsAfter {
		t.Fatal("task should not exist in history after ClearHistory")
	}
}

func TestSieve_ConsecutiveDetection_StillWorks(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3
	taskID := "task-consecutive-test"

	call := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "/tmp/a"}}

	for i := 0; i < 3; i++ {
		valid, _ := s.InspectToolCalls(taskID, []store.ToolInteraction{call})
		if !valid {
			t.Fatalf("call %d should be valid (at or below threshold)", i)
		}
	}

	valid, reason := s.InspectToolCalls(taskID, []store.ToolInteraction{call})
	if valid {
		t.Fatal("fourth consecutive call should be detected as loop")
	}
	if reason != "InfiniteLoopDetected" {
		t.Fatalf("expected InfiniteLoopDetected, got %s", reason)
	}
}

func TestSieve_WindowDetection_DoesNotFlagNormalUsage(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3
	taskID := "task-normal-usage"

	calls := []store.ToolInteraction{
		{ToolName: "read_file", Arguments: map[string]any{"path": "/tmp/a"}},
		{ToolName: "write_file", Arguments: map[string]any{"path": "/tmp/b"}},
		{ToolName: "read_file", Arguments: map[string]any{"path": "/tmp/c"}},
		{ToolName: "write_file", Arguments: map[string]any{"path": "/tmp/d"}},
	}

	for _, call := range calls {
		valid, _ := s.InspectToolCalls(taskID, []store.ToolInteraction{call})
		if !valid {
			t.Fatalf("normal varied usage should not be flagged as loop: %s", call.ToolName)
		}
	}
}
