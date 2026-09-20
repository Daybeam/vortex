package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

func TestJITManager_GetSource(t *testing.T) {
	tempDir := t.TempDir()
	// Use manual registry initialization to avoid dependency on config.NewTestRegistry if it doesn't exist
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	script := "print('hello')"
	id, err := jit.RegisterTool(script, "python", 1*time.Minute, true)
	if err != nil {
		t.Fatalf("RegisterTool failed: %v", err)
	}

	source, err := jit.GetSource(id)
	if err != nil {
		t.Fatalf("GetSource failed: %v", err)
	}

	if source != script {
		t.Errorf("expected source %q, got %q", script, source)
	}
}

func TestJITManager_GetSource_NotFound(t *testing.T) {
	tempDir := t.TempDir()
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	jit := NewJITManager(reg, tempDir)

	_, err := jit.GetSource("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent tool, got nil")
	}
}
