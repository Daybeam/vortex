package core

import (
	"testing"
	"unicode/utf8"
)

// TestC15_SpawnerReasonString_NoMojibake is a regression test for audit C-15:
// the spawner's tool-call reason string must be valid UTF-8 with no mojibake
// (garbled multi-byte characters from encoding mismatches).
//
// Before the fix, the string literal contained mojibake characters (garbled
// bytes from a Windows codepage mismatch), which caused log corruption and
// potential JSON encoding failures downstream.
// After the fix, the string is "tool call (LLM initiated)" — pure ASCII.
//
// fixes audit T-C03: the original test compared two identical local literals
// and never referenced production code. Now it checks the production constant
// ToolCallReasonLLMInitiated, so it will FAIL if the constant reverts to mojibake.
func TestC15_SpawnerReasonString_NoMojibake(t *testing.T) {
	reason := ToolCallReasonLLMInitiated // reference production, not a local literal

	// Must be valid UTF-8.
	if !utf8.ValidString(reason) {
		t.Errorf("reason string is not valid UTF-8: %q", reason)
	}

	// Must be pure ASCII (no mojibake).
	for _, r := range reason {
		if r > 127 {
			t.Errorf("reason string contains non-ASCII rune %U (mojibake): %q", r, reason)
		}
	}

	// Must match the expected value exactly.
	const expected = "tool call (LLM initiated)"
	if reason != expected {
		t.Errorf("reason string mismatch: got %q, want %q", reason, expected)
	}
}
