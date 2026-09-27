package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestR4_EmbeddedRunner_LuaCancellationDrainsChannel is a regression test for
// audit R-4: runGopherLua must drain outCh in the cancellation path before
// returning, to ensure the goroutine has fully exited before L.Close() runs
// (use-after-close race).
//
// Before the fix, the cancellation path returned immediately without draining
// outCh. The goroutine running L.DoString could still be executing when the
// caller proceeded to close the Lua VM, causing a use-after-close race.
// After the fix, `<-outCh` waits for the goroutine to send its result.
//
// Reproduction: run a long-running Lua script with a short timeout context.
// The function should return with a cancellation error (not panic or hang).
func TestR4_EmbeddedRunner_LuaCancellationDrainsChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Run a Lua script that loops long enough to be cancelled.
	// The script must not terminate before the context timeout.
	result, err := EmbeddedRunner(ctx, "lua", `
		local i = 0
		while true do
			i = i + 1
		end
		return i
	`)

	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}

	// The error should mention cancellation.
	if !strings.Contains(err.Error(), "cancel") && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected cancellation error, got: %v", err)
	}

	// The result should still be returned (not nil).
	if result == nil {
		t.Error("expected non-nil result even on cancellation")
	}
}

// TestR4_EmbeddedRunner_LuaNormalCompletion verifies that normal (non-cancelled)
// Lua execution still works after the R-4 fix.
func TestR4_EmbeddedRunner_LuaNormalCompletion(t *testing.T) {
	ctx := context.Background()

	result, err := EmbeddedRunner(ctx, "lua", `result = 42`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Lua numbers come back as float64.
	switch v := result.Result.(type) {
	case float64:
		if v != 42 {
			t.Errorf("expected result 42, got %v", v)
		}
	case int:
		if v != 42 {
			t.Errorf("expected result 42, got %v", v)
		}
	case int64:
		if v != 42 {
			t.Errorf("expected result 42, got %v", v)
		}
	default:
		t.Errorf("expected result 42 (numeric), got %T: %v", result.Result, result.Result)
	}
}
