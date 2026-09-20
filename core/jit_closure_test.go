package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// jit_closure_test.go — tests for Task-Local Closure (architecture §二.1).
// Verifies the snapshot/restore lifecycle: JIT tools are snapshotted at
// submission time and can be restored after global TTL expiry.

func TestJITManager_SnapshotJITTools(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	// Register a JIT tool.
	id, err := jit.RegisterTool("print('hello')", "python", 1*time.Minute, true)
	if err != nil {
		t.Fatalf("RegisterTool failed: %v", err)
	}

	// Snapshot it.
	snap := jit.SnapshotJITTools([]string{id, "nonexistent_id"})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snap) != 1 {
		t.Errorf("expected 1 entry in snapshot, got %d", len(snap))
	}
	if _, ok := snap[id]; !ok {
		t.Errorf("expected snapshot to contain %q", id)
	}

	// Verify the snapshot contains a valid MCPDef.
	var mcp config.MCPDef
	if err := json.Unmarshal(snap[id], &mcp); err != nil {
		t.Fatalf("snapshot data is not a valid MCPDef: %v", err)
	}
	if mcp.ID != id {
		t.Errorf("expected snapshot ID %q, got %q", id, mcp.ID)
	}
}

func TestJITManager_SnapshotJITTools_EmptyInput(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	if snap := jit.SnapshotJITTools(nil); snap != nil {
		t.Error("expected nil snapshot for nil input")
	}
	if snap := jit.SnapshotJITTools([]string{}); snap != nil {
		t.Error("expected nil snapshot for empty input")
	}
}

func TestJITManager_RestoreLocalMCPs_AfterGlobalExpiry(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	// Register a JIT tool.
	id, err := jit.RegisterTool("print('hello')", "python", 1*time.Minute, true)
	if err != nil {
		t.Fatalf("RegisterTool failed: %v", err)
	}

	// Snapshot it (simulating task submission).
	snap := jit.SnapshotJITTools([]string{id})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	// Simulate global TTL expiry: remove the tool from the global registry.
	reg.Mu.Lock()
	delete(reg.DynamicMCPs, id)
	reg.Mu.Unlock()

	// Verify the tool is gone.
	if reg.GetMCP(id) != nil {
		t.Fatal("expected tool to be gone after simulated expiry")
	}

	// Restore from snapshot (simulating executeStep).
	restored := jit.RestoreLocalMCPs(snap)
	if len(restored) != 1 || restored[0] != id {
		t.Errorf("expected restored=[%q], got %v", id, restored)
	}

	// Verify the tool is back.
	mcp := reg.GetMCP(id)
	if mcp == nil {
		t.Fatal("expected tool to be restored after RestoreLocalMCPs")
	}
	if mcp.ID != id {
		t.Errorf("expected restored ID %q, got %q", id, mcp.ID)
	}
}

func TestJITManager_RestoreLocalMCPs_NoClobberWhenAlive(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	// Register and snapshot.
	id, err := jit.RegisterTool("print('original')", "python", 1*time.Minute, true)
	if err != nil {
		t.Fatalf("RegisterTool failed: %v", err)
	}
	snap := jit.SnapshotJITTools([]string{id})

	// Tool is still alive globally — restore should be a no-op.
	restored := jit.RestoreLocalMCPs(snap)
	if len(restored) != 0 {
		t.Errorf("expected no restores when tool is still alive, got %v", restored)
	}
}

func TestJITManager_RestoreLocalMCPs_EmptySnapshot(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	if restored := jit.RestoreLocalMCPs(nil); restored != nil {
		t.Error("expected nil for nil snapshot")
	}
	if restored := jit.RestoreLocalMCPs(map[string]json.RawMessage{}); restored != nil {
		t.Error("expected nil for empty snapshot")
	}
}

func TestJITManager_ClosureRoundTrip_EmbeddedLang(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	// Register an embedded JS tool.
	id, err := jit.RegisterTool("console.log('hello')", "js", 1*time.Minute, true)
	if err != nil {
		t.Fatalf("RegisterTool failed: %v", err)
	}

	// Snapshot → expire → restore → verify.
	snap := jit.SnapshotJITTools([]string{id})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	reg.Mu.Lock()
	delete(reg.DynamicMCPs, id)
	reg.Mu.Unlock()

	restored := jit.RestoreLocalMCPs(snap)
	if len(restored) != 1 {
		t.Fatalf("expected 1 restore, got %d", len(restored))
	}

	mcp := reg.GetMCP(id)
	if mcp == nil {
		t.Fatal("expected restored tool to be available")
	}
	// Verify it's still an embedded marker.
	if mcp.Command == "" {
		t.Error("expected non-empty command on restored tool")
	}
}
