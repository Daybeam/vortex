package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// ─── TurnIdempotencyContext Tests ─────────────────────────────────────────

func TestTurnIdempotencyContext_FirstCall_Proceeds(t *testing.T) {
	ctx := NewTurnIdempotencyContext()
	key := "write_file:12345"

	if ctx.CheckAndMark(key) {
		t.Error("first call should not be marked as executed")
	}
}

func TestTurnIdempotencyContext_SecondCall_Skipped(t *testing.T) {
	ctx := NewTurnIdempotencyContext()
	key := "write_file:12345"

	ctx.CheckAndMark(key) // first call
	if !ctx.CheckAndMark(key) {
		t.Error("second call with same key should be skipped")
	}
}

func TestTurnIdempotencyContext_DifferentKeys_BothProceed(t *testing.T) {
	ctx := NewTurnIdempotencyContext()

	if ctx.CheckAndMark("write_file:aaa") {
		t.Error("first call to key aaa should proceed")
	}
	if ctx.CheckAndMark("write_file:bbb") {
		t.Error("first call to key bbb should proceed")
	}
}

func TestTurnIdempotencyContext_HasExecuted(t *testing.T) {
	ctx := NewTurnIdempotencyContext()
	key := "read_file:67890"

	if ctx.HasExecuted(key) {
		t.Error("key should not be executed before MarkExecuted")
	}
	ctx.MarkExecuted(key)
	if !ctx.HasExecuted(key) {
		t.Error("key should be executed after MarkExecuted")
	}
}

func TestTurnIdempotencyContext_NewContext_EmptyState(t *testing.T) {
	ctx1 := NewTurnIdempotencyContext()
	ctx1.CheckAndMark("write_file:aaa")

	// New context should not share state with old context.
	ctx2 := NewTurnIdempotencyContext()
	if ctx2.HasExecuted("write_file:aaa") {
		t.Error("new context should not have any executed keys")
	}
}

func TestTurnIdempotencyContext_ConcurrentAccess(t *testing.T) {
	ctx := NewTurnIdempotencyContext()
	key := "execute_code:shared"

	// Simulate concurrent access — should not panic.
	done := make(chan bool, 2)
	go func() {
		ctx.CheckAndMark(key)
		done <- true
	}()
	go func() {
		ctx.CheckAndMark(key)
		done <- true
	}()
	<-done
	<-done

	// After concurrent access, the key should be marked.
	if !ctx.HasExecuted(key) {
		t.Error("key should be executed after concurrent access")
	}
}

// ─── A5 Dry-Run Helper Tests ──────────────────────────────────────────────

func TestSupportsDryRun_HasDryRunInSchema_ReturnsTrue(t *testing.T) {
	toolDef := &schemas.ToolDefinition{
		Name: "dangerous_tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"param":   map[string]any{"type": "string"},
				"dry_run": map[string]any{"type": "boolean"},
			},
		},
	}
	if !supportsDryRun(toolDef) {
		t.Error("tool with dry_run in schema should support dry_run")
	}
}

func TestSupportsDryRun_NoDryRunInSchema_ReturnsFalse(t *testing.T) {
	toolDef := &schemas.ToolDefinition{
		Name: "safe_tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"param": map[string]any{"type": "string"},
			},
		},
	}
	if supportsDryRun(toolDef) {
		t.Error("tool without dry_run in schema should not support dry_run")
	}
}

func TestSupportsDryRun_NilDef_ReturnsFalse(t *testing.T) {
	if supportsDryRun(nil) {
		t.Error("nil tool def should not support dry_run")
	}
}

func TestSupportsDryRun_NilSchema_ReturnsFalse(t *testing.T) {
	toolDef := &schemas.ToolDefinition{Name: "tool"}
	if supportsDryRun(toolDef) {
		t.Error("nil InputSchema should not support dry_run")
	}
}

func TestIsHighRiskTool_HighRisk_ReturnsTrue(t *testing.T) {
	toolDef := &schemas.ToolDefinition{
		Name:      "dangerous_tool",
		RiskLevel: "high",
	}
	if !isHighRiskTool(toolDef) {
		t.Error("tool with RiskLevel=high should be high risk")
	}
}

func TestIsHighRiskTool_LowRisk_ReturnsFalse(t *testing.T) {
	toolDef := &schemas.ToolDefinition{
		Name:      "safe_tool",
		RiskLevel: "low",
	}
	if isHighRiskTool(toolDef) {
		t.Error("tool with RiskLevel=low should not be high risk")
	}
}

func TestIsHighRiskTool_NilDef_ReturnsFalse(t *testing.T) {
	if isHighRiskTool(nil) {
		t.Error("nil tool def should not be high risk")
	}
}

func TestFindCoreToolDef_ExistingTool_ReturnsDef(t *testing.T) {
	def := findCoreToolDef("write_file")
	if def == nil {
		t.Fatal("expected non-nil def for write_file")
	}
	if def.Name != "write_file" {
		t.Errorf("expected write_file, got %s", def.Name)
	}
}

func TestFindCoreToolDef_NonexistentTool_ReturnsNil(t *testing.T) {
	def := findCoreToolDef("nonexistent_tool")
	if def != nil {
		t.Error("expected nil for nonexistent tool")
	}
}

func TestAddDryRunArg_PreservesExistingArgs(t *testing.T) {
	original := map[string]any{
		"path":    "test.txt",
		"content": "hello",
	}
	result := addDryRunArg(original)

	if result["dry_run"] != true {
		t.Error("dry_run should be true")
	}
	if result["path"] != "test.txt" {
		t.Error("original path should be preserved")
	}
	if result["content"] != "hello" {
		t.Error("original content should be preserved")
	}
	// Original should not be modified.
	if _, exists := original["dry_run"]; exists {
		t.Error("original args should not be modified")
	}
}

func TestAddDryRunArg_EmptyArgs(t *testing.T) {
	result := addDryRunArg(map[string]any{})
	if result["dry_run"] != true {
		t.Error("dry_run should be true")
	}
	if len(result) != 1 {
		t.Errorf("expected 1 key, got %d", len(result))
	}
}

func TestDryRunResult_ContainsToolNameAndPreview(t *testing.T) {
	result := dryRunResult("write_file", "Written 100 bytes to test.txt")
	if !strings.Contains(result, "DRY-RUN PREVIEW") {
		t.Error("should contain DRY-RUN PREVIEW marker")
	}
	if !strings.Contains(result, "write_file") {
		t.Error("should contain tool name")
	}
	if !strings.Contains(result, "Written 100 bytes") {
		t.Error("should contain original result")
	}
	if !strings.Contains(result, "NOT actually executed") {
		t.Error("should warn that operation was not executed")
	}
}

func TestMarshalArgsForLog_ValidArgs(t *testing.T) {
	args := map[string]any{"path": "test.txt", "content": "hello"}
	result := marshalArgsForLog(args)
	if !strings.Contains(result, "test.txt") {
		t.Error("should contain path value")
	}
}

func TestMarshalArgsForLog_NilArgs(t *testing.T) {
	result := marshalArgsForLog(nil)
	if result != "null" {
		t.Errorf("expected 'null' for nil args, got %s", result)
	}
}

// ─── Integration Test: Core Tools RiskLevel ──────────────────────────────

func TestCoreTools_NoneAreHighRisk(t *testing.T) {
	// Current core tools (write_file, read_file, execute_code, decompress)
	// should not have RiskLevel=high — they're all local operations.
	for _, def := range CoreToolDefinitions() {
		if isHighRiskTool(&def) {
			t.Errorf("core tool %s should not be high risk", def.Name)
		}
	}
}
