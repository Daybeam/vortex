package core

import (
	"testing"
)

// TestRegexCache_SizeCapPreservesTriggerEntry verifies that when the regex
// cache exceeds its size limit, the entry that triggered the clear is not
// lost. Before the fix, a TOCTOU race between counting entries and clearing
// could delete entries that were added between the two operations.
func TestRegexCache_SizeCapPreservesTriggerEntry(t *testing.T) {
	// Reset the cache for a clean test.
	regexCache.Range(func(k, _ any) bool {
		regexCache.Delete(k)
		return true
	})
	defer func() {
		regexCache.Range(func(k, _ any) bool {
			regexCache.Delete(k)
			return true
		})
	}()

	// Fill cache to just under the limit.
	for i := 0; i < regexCacheMaxEntries-1; i++ {
		compileRegexCache("pattern-" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}

	// Add one more to trigger the size check. This should not lose entries.
	re, err := compileRegexCache("trigger-pattern")
	if err != nil {
		t.Fatalf("compileRegexCache failed: %v", err)
	}
	if re == nil {
		t.Fatal("compileRegexCache returned nil regex")
	}

	// The trigger pattern must still be in the cache after the clear.
	if v, ok := regexCache.Load("trigger-pattern"); !ok || v == nil {
		t.Error("trigger-pattern was lost from cache during count-and-clear — TOCTOU race")
	}
}
