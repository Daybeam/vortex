package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestCompactOldObservations_NoCompactionWithinBudget verifies that when
// the number of observation blocks is within the budget, no compaction
// occurs. This is the baseline — short tasks must behave exactly as before.
//
// See docs/plan/VCP_HEARTBEAT_AND_CONTEXT_TIERING_DESIGN.md §3 Bucket 2.
func TestCompactOldObservations_NoCompactionWithinBudget(t *testing.T) {
	blocks := []schemas.ContentBlock{
		{Text: "I will run the tests."},
		{Text: "[ENVIRONMENT OBSERVATION]\nrun_command: npm test\nAll 10 tests passed."},
		{Text: "Tests passed. Let me check coverage."},
		{Text: "[ENVIRONMENT OBSERVATION]\nrun_command: npm coverage\nCoverage: 85%."},
	}

	result := CompactOldObservations(blocks, 5)
	if len(result) != len(blocks) {
		t.Fatalf("expected %d blocks, got %d (no compaction should occur)", len(blocks), len(result))
	}
	for i := range blocks {
		if result[i].Text != blocks[i].Text {
			t.Errorf("block %d text changed: got %q, want %q", i, result[i].Text, blocks[i].Text)
		}
	}
}

// TestCompactOldObservations_CompactsOldBlocks verifies that when the
// number of observation blocks exceeds the budget, the oldest ones are
// replaced with compact summaries while the latest ones are kept in full.
func TestCompactOldObservations_CompactsOldBlocks(t *testing.T) {
	// Build 8 turns: 8 assistant + 8 observation = 16 blocks.
	// With maxFull=3, the oldest 5 observations should be compacted.
	blocks := make([]schemas.ContentBlock, 0, 16)
	for i := 1; i <= 8; i++ {
		blocks = append(blocks, schemas.ContentBlock{
			Text: "Assistant turn " + itoa(i),
		})
		blocks = append(blocks, schemas.ContentBlock{
			Text: "[ENVIRONMENT OBSERVATION]\nrun_command: step" + itoa(i) + "\nOutput line " + itoa(i) + " with some content that is long enough to be meaningful.",
		})
	}

	result := CompactOldObservations(blocks, 3)

	// Count compacted vs full observations.
	compacted := 0
	full := 0
	for _, b := range result {
		if strings.HasPrefix(b.Text, MarkerEnvObservation) {
			if strings.Contains(b.Text, "summarized") {
				compacted++
			} else {
				full++
			}
		}
	}

	if compacted != 5 {
		t.Errorf("expected 5 compacted observations, got %d", compacted)
	}
	if full != 3 {
		t.Errorf("expected 3 full observations, got %d", full)
	}

	// Assistant turns must all be preserved.
	assistantCount := 0
	for _, b := range result {
		if strings.HasPrefix(b.Text, "Assistant turn ") {
			assistantCount++
		}
	}
	if assistantCount != 8 {
		t.Errorf("expected 8 assistant turns preserved, got %d", assistantCount)
	}
}

// TestCompactOldObservations_PreservesLatest verifies that the latest
// observations are kept in full (not compacted).
func TestCompactOldObservations_PreservesLatest(t *testing.T) {
	blocks := make([]schemas.ContentBlock, 0, 10)
	for i := 1; i <= 5; i++ {
		blocks = append(blocks, schemas.ContentBlock{Text: "turn " + itoa(i)})
		blocks = append(blocks, schemas.ContentBlock{
			Text: "[ENVIRONMENT OBSERVATION]\nstep " + itoa(i) + " output",
		})
	}

	result := CompactOldObservations(blocks, 2)

	// The last 2 observations should be in full.
	obsTexts := make([]string, 0)
	for _, b := range result {
		if strings.HasPrefix(b.Text, MarkerEnvObservation) {
			obsTexts = append(obsTexts, b.Text)
		}
	}

	if len(obsTexts) != 5 {
		t.Fatalf("expected 5 observation blocks, got %d", len(obsTexts))
	}

	// Last 2 should contain "step 4" and "step 5" (full content).
	if !strings.Contains(obsTexts[3], "step 4 output") {
		t.Errorf("4th observation should be full (step 4), got: %s", obsTexts[3])
	}
	if !strings.Contains(obsTexts[4], "step 5 output") {
		t.Errorf("5th observation should be full (step 5), got: %s", obsTexts[4])
	}

	// First 3 should be compacted.
	for i := 0; i < 3; i++ {
		if !strings.Contains(obsTexts[i], "summarized") {
			t.Errorf("observation %d should be compacted, got: %s", i, obsTexts[i])
		}
	}
}

// TestCompactOldObservations_DoesNotMutateInput verifies the function
// is pure — it must not modify the input slice.
func TestCompactOldObservations_DoesNotMutateInput(t *testing.T) {
	original := []schemas.ContentBlock{
		{Text: "turn 1"},
		{Text: "[ENVIRONMENT OBSERVATION]\noutput 1"},
		{Text: "turn 2"},
		{Text: "[ENVIRONMENT OBSERVATION]\noutput 2"},
		{Text: "turn 3"},
		{Text: "[ENVIRONMENT OBSERVATION]\noutput 3"},
	}
	// Snapshot original text values.
	originalTexts := make([]string, len(original))
	for i, b := range original {
		originalTexts[i] = b.Text
	}

	_ = CompactOldObservations(original, 1)

	for i, expected := range originalTexts {
		if original[i].Text != expected {
			t.Errorf("input block %d was mutated: got %q, want %q", i, original[i].Text, expected)
		}
	}
}

// TestExtractFirstLine verifies the summary extraction helper.
func TestExtractFirstLine(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "single line after marker",
			input:    "[ENVIRONMENT OBSERVATION]\nrun_command: npm test",
			expected: "run_command: npm test",
		},
		{
			name:     "multi line takes first",
			input:    "[ENVIRONMENT OBSERVATION]\nline1\nline2\nline3",
			expected: "line1",
		},
		{
			name:     "long line truncated",
			input:    "[ENVIRONMENT OBSERVATION]\n" + strings.Repeat("x", 200),
			expected: strings.Repeat("x", 117) + "...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFirstLine(tc.input)
			if got != tc.expected {
				t.Errorf("got %q, want %q", got, tc.expected)
			}
		})
	}
}

// itoa is a minimal int-to-string helper to avoid importing strconv in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
