package core

import (
	"testing"
)

// Feature: Sieve Guardian
// Scenario: Repetition detection — when output repetition rate exceeds threshold,
//          the call is intercepted
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Sieve Guardian
// -----------------------------------------------------------------------------
func TestSieve_BlocksWhenRepetitionExceedsThreshold(t *testing.T) {
	s := NewSieve(10)
	s.WindowSize = 2
	s.RepetitionThreshold = 0.9

	text := "line 1\nline 2\nline 1\nline 2"
	valid, reason := s.Inspect(text, "")
	if valid {
		t.Errorf("Expected invalid due to repetition, got valid")
	}
	if reason != "repetition detected: output entered a logic loop" {
		t.Errorf("Expected repetition reason, got %q", reason)
	}
}
