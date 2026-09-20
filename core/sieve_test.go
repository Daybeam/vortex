package core

import (
	"testing"
)

func TestSieveInspectRepetition(t *testing.T) {
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
