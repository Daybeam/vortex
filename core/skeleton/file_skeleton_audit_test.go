package skeleton

import (
	"testing"
)

// TestJsonSkeleton_UnmatchedBracketNoPanic is a regression test for audit fix F.
// The skeletonizer processes arbitrary tool output which may contain stray ']'
// or '}' brackets that are not valid JSON. Before the fix, an unmatched ']'
// caused an index-out-of-range panic on an empty arrayCount slice. After the
// fix, the stray bracket is skipped and the function returns safely.
func TestJsonSkeleton_UnmatchedBracketNoPanic(t *testing.T) {
	// Input with a stray ']' that has no matching '[' — this would panic
	// before the fix because arrayCount/arrayStartLine were empty.
	inputs := [][]byte{
		[]byte("some text ] trailing"),
		[]byte("} stray close brace"),
		[]byte("]"),
		[]byte("}"),
		[]byte("mixed ] and } junk"),
	}
	for i, in := range inputs {
		// Must not panic.
		_ = jsonSkeleton(in)
		// Also exercise the public entry point.
		_ = FileSkeleton(in, "test.txt")
		_ = i
	}
}

// TestJsonSkeleton_ValidArrayStillWorks ensures the fix did not break valid
// JSON array skeletonization (the guard must not skip legitimate ']' pops).
func TestJsonSkeleton_ValidArrayStillWorks(t *testing.T) {
	out := jsonSkeleton([]byte(`{"items": ["a", "b", "c"], "count": 3}`))
	if out == "" {
		t.Error("expected non-empty skeleton for valid JSON array input")
	}
}
