package providers

import "testing"

// TestSetDisableThinking verifies the atomic flag can be toggled at runtime.
func TestSetDisableThinking(t *testing.T) {
	defer SetDisableThinking(false)

	SetDisableThinking(true)
	if !disableThinkingFlag.Load() {
		t.Error("disableThinkingFlag should be true after SetDisableThinking(true)")
	}

	SetDisableThinking(false)
	if disableThinkingFlag.Load() {
		t.Error("disableThinkingFlag should be false after SetDisableThinking(false)")
	}
}

// TestSetDebugEnabled verifies the atomic flag can be toggled at runtime.
func TestSetDebugEnabled(t *testing.T) {
	defer SetDebugEnabled(false)

	SetDebugEnabled(true)
	if !debugEnabledFlag.Load() {
		t.Error("debugEnabledFlag should be true after SetDebugEnabled(true)")
	}

	SetDebugEnabled(false)
	if debugEnabledFlag.Load() {
		t.Error("debugEnabledFlag should be false after SetDebugEnabled(false)")
	}
}
