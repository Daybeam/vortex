package core

import (
	"strings"
	"testing"
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

func TestTurnIdempotencyContext_NewContext_EmptyState(t *testing.T) {
	ctx1 := NewTurnIdempotencyContext()
	ctx1.CheckAndMark("write_file:aaa")

	// New context should not share state with old context.
	ctx2 := NewTurnIdempotencyContext()
	if ctx2.CheckAndMark("write_file:aaa") {
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
	if !ctx.CheckAndMark(key) {
		t.Error("key should be executed after concurrent access")
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

// ─── O46 Invariant: Core Tools Have No A5 Dry-Run Trigger ────────────────

func TestCoreTools_NoHighRiskOrDryRun(t *testing.T) {
	// O46 removed the unreachable A5 dispatch branch because no core tool was
	// RiskLevel=high or exposed dry_run, and no core handler consumed dry_run.
	// Keep that precondition pinned so a real preview cannot be mistaken for a
	// committed operation (or relabeled after execution).
	for _, def := range CoreToolDefinitions() {
		if def.RiskLevel == "high" {
			t.Errorf("O46 invariant: core tool %q exposes RiskLevel=high", def.Name)
		}
		if def.InputSchema != nil {
			if props, ok := def.InputSchema["properties"].(map[string]any); ok {
				if _, hasDryRun := props["dry_run"]; hasDryRun {
					t.Errorf("O46 invariant: core tool %q exposes dry_run", def.Name)
				}
			}
		}
	}
}
