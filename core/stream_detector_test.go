package core

import (
	"strings"
	"testing"
)

// distinct65 is a 65-byte string with all distinct characters. When repeated,
// the accumulated text has period=65 > maxPeriod=64, so only the consecutive
// detection mode can fire (not short-period).
const distinct65 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#"

// TestStreamChunkDetector_ConsecutiveIdenticalChunks verifies that >= 60
// consecutive identical chunks trigger ErrRepetitiveOutput.
//
// Uses distinct65 (period=65 > maxPeriod) so only consecutive mode fires.
func TestStreamChunkDetector_ConsecutiveIdenticalChunks(t *testing.T) {
	d := NewStreamChunkDetector()
	for i := 0; i < 59; i++ {
		if err := d.Check(distinct65); err != nil {
			t.Fatalf("unexpected error at chunk %d: %v", i, err)
		}
	}
	// 60th identical chunk should trigger
	if err := d.Check(distinct65); err != ErrRepetitiveOutput {
		t.Fatalf("expected ErrRepetitiveOutput at 60th consecutive chunk, got %v", err)
	}
}

// TestStreamChunkDetector_ConsecutiveResetByDifferentChunk verifies that a
// different chunk resets the consecutive counter.
func TestStreamChunkDetector_ConsecutiveResetByDifferentChunk(t *testing.T) {
	d := NewStreamChunkDetector()
	for i := 0; i < 59; i++ {
		if err := d.Check(distinct65); err != nil {
			t.Fatalf("unexpected error at chunk %d: %v", i, err)
		}
	}
	// Different chunk resets counter
	if err := d.Check("different"); err != nil {
		t.Fatalf("unexpected error on different chunk: %v", err)
	}
	// Now 59 more identical chunks should not trigger (counter restarted)
	for i := 0; i < 59; i++ {
		if err := d.Check(distinct65); err != nil {
			t.Fatalf("unexpected error after reset at chunk %d: %v", i, err)
		}
	}
}

// TestStreamChunkDetector_ShortPeriodCycle verifies that a short-period
// cycle (period <= 64, span >= 256) triggers ErrRepetitiveOutput.
//
// Uses alternating "AB"/"CD" chunks so consecutive detection never fires
// (different hashes each time), isolating the short-period mode. The text
// becomes "ABCDABCD..." with period=4; after 128 chunks span=256.
func TestStreamChunkDetector_ShortPeriodCycle(t *testing.T) {
	d := NewStreamChunkDetector()
	for i := 0; i < 200; i++ {
		chunk := "AB"
		if i%2 == 1 {
			chunk = "CD"
		}
		err := d.Check(chunk)
		if err == ErrRepetitiveOutput {
			return // detected as expected
		}
		if err != nil {
			t.Fatalf("unexpected error at chunk %d: %v", i, err)
		}
	}
	t.Fatal("expected ErrRepetitiveOutput from short-period cycle (period=4, span>=256)")
}

// TestStreamChunkDetector_ShortPeriodSingleChar verifies that alternating
// single chars "xyxy..." (period=2) triggers when span >= 256.
func TestStreamChunkDetector_ShortPeriodSingleChar(t *testing.T) {
	d := NewStreamChunkDetector()
	for i := 0; i < 300; i++ {
		chunk := "x"
		if i%2 == 1 {
			chunk = "y"
		}
		err := d.Check(chunk)
		if err == ErrRepetitiveOutput {
			return
		}
		if err != nil {
			t.Fatalf("unexpected error at char %d: %v", i, err)
		}
	}
	t.Fatal("expected ErrRepetitiveOutput from alternating single-char pattern")
}

// TestStreamChunkDetector_LongerPeriod verifies that a period of 64 (the
// maximum) triggers when span >= 256 (4 repetitions).
func TestStreamChunkDetector_LongerPeriod(t *testing.T) {
	d := NewStreamChunkDetector()
	pattern := strings.Repeat("A", 64) // period = 64
	for i := 0; i < 3; i++ {
		if err := d.Check(pattern); err != nil {
			t.Fatalf("unexpected error at repetition %d: %v", i, err)
		}
	}
	// 4th repetition: span = 256, should trigger
	if err := d.Check(pattern); err != ErrRepetitiveOutput {
		t.Fatalf("expected ErrRepetitiveOutput at span=256 with period=64, got %v", err)
	}
}

// TestStreamChunkDetector_NormalVariedOutputNoTrigger verifies that normal,
// varied text output does NOT trigger false positives.
func TestStreamChunkDetector_NormalVariedOutputNoTrigger(t *testing.T) {
	d := NewStreamChunkDetector()
	chunks := []string{
		"The quick brown fox jumps over the lazy dog. ",
		"Lorem ipsum dolor sit amet, consectetur adipiscing elit. ",
		"Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. ",
		"Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris. ",
		"Nisi ut aliquip ex ea commodo consequat. Duis aute irure dolor. ",
		"In reprehenderit in voluptate velit esse cillum dolore eu fugiat. ",
		"Excepteur sint occaecat cupidatat non proident, sunt in culpa. ",
		"Qui officia deserunt mollit anim id est laborum. Sed ut perspiciatis. ",
		"Unde omnis iste natus error sit voluptatem accusantium doloremque. ",
		"Laudantium, totam rem aperiam, eaque ipsa quae ab illo inventore. ",
	}
	for i, chunk := range chunks {
		if err := d.Check(chunk); err != nil {
			t.Fatalf("false positive at chunk %d: %v", i, err)
		}
	}
}

// TestStreamChunkDetector_PeriodTooLongNoTrigger verifies that a pattern
// with period > 64 and all-distinct characters does NOT trigger short-period
// detection. Consecutive detection also doesn't fire because chunks differ.
func TestStreamChunkDetector_PeriodTooLongNoTrigger(t *testing.T) {
	d := NewStreamChunkDetector()
	// Feed 4 repetitions of distinct65 (period=65 > maxPeriod=64).
	// Chunks are identical, so consecutive count = 4 (< 60, no trigger).
	// Short-period can't find any period <= 64, so no trigger.
	for i := 0; i < 4; i++ {
		if err := d.Check(distinct65); err != nil {
			t.Fatalf("unexpected error at repetition %d: %v", i, err)
		}
	}
}

// TestStreamChunkDetector_BufferTrimming verifies that the detector handles
// large outputs without crashing and still detects repetition in the tail.
func TestStreamChunkDetector_BufferTrimming(t *testing.T) {
	d := NewStreamChunkDetector()
	// Feed a large varied prefix (well beyond bufCap=4096)
	for i := 0; i < 200; i++ {
		chunk := "This is line " + string(rune('A'+i%26)) + " of varied output. "
		if err := d.Check(chunk); err != nil {
			t.Fatalf("unexpected error in prefix at line %d: %v", i, err)
		}
	}
	// Now feed a repetitive tail that should still be detected.
	// Use alternating chunks to avoid consecutive detection.
	for i := 0; i < 300; i++ {
		chunk := "XY"
		if i%2 == 1 {
			chunk = "ZW"
		}
		err := d.Check(chunk)
		if err == ErrRepetitiveOutput {
			return // detected as expected
		}
		if err != nil {
			t.Fatalf("unexpected non-repetitive error: %v", err)
		}
	}
	t.Fatal("expected ErrRepetitiveOutput from repetitive tail after large prefix")
}

// TestStreamChunkDetector_EmptyChunks verifies that empty chunks don't
// cause panics. 60+ identical empty chunks will trigger consecutive
// detection (same hash), which is correct behavior.
func TestStreamChunkDetector_EmptyChunks(t *testing.T) {
	d := NewStreamChunkDetector()
	for i := 0; i < 59; i++ {
		if err := d.Check(""); err != nil {
			t.Fatalf("unexpected error on empty chunk %d: %v", i, err)
		}
	}
	// 60th empty chunk: consecutive identical hashes → trigger
	if err := d.Check(""); err != ErrRepetitiveOutput {
		t.Fatalf("expected ErrRepetitiveOutput at 60th empty chunk, got %v", err)
	}
}

// TestStreamChunkDetector_PerCallIsolation verifies that two separate
// detectors don't interfere with each other (per-call design).
func TestStreamChunkDetector_PerCallIsolation(t *testing.T) {
	d1 := NewStreamChunkDetector()
	d2 := NewStreamChunkDetector()
	for i := 0; i < 59; i++ {
		_ = d1.Check(distinct65)
	}
	// d1 is at 59 consecutive, d2 is fresh
	if err := d2.Check(distinct65); err != nil {
		t.Fatalf("d2 should not be affected by d1: %v", err)
	}
	// d1 should trigger on next
	if err := d1.Check(distinct65); err != ErrRepetitiveOutput {
		t.Fatalf("d1 should trigger: %v", err)
	}
}
