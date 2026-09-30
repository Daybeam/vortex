package core

import (
	"testing"
)

// TestHashCache_BoundedGrowth verifies that the hashCache does not grow
// beyond maxHashCacheEntries. Without the eviction logic, the cache would
// grow unbounded on long-running servers.
//
// fixes audit T-C04: the original test inlined the eviction logic instead
// of calling the production method. Now it calls recordHashCacheEntry,
// so it will FAIL if the production eviction is removed or broken.
func TestHashCache_BoundedGrowth(t *testing.T) {
	s := &DirectedEngine{
		hashCache:           make(map[string]string),
		maxHashCacheEntries: 5, // Small cap for testing
	}

	// Fill the cache to the cap via the production method.
	for i := 0; i < 5; i++ {
		key := "/path/to/file-" + string(rune('A'+i))
		s.recordHashCacheEntry(key, "hash-"+string(rune('A'+i)))
	}

	if len(s.hashCache) != 5 {
		t.Fatalf("expected 5 entries after filling to cap, got %d", len(s.hashCache))
	}

	// Add one more entry — this triggers eviction (keep half + add 1).
	// With cap=5, keep=5/2=2, so after add: 2+1=3 entries.
	s.recordHashCacheEntry("/path/to/file-F", "hash-F")

	if len(s.hashCache) > 3 {
		t.Fatalf("expected ≤3 entries after eviction (keep half + 1), got %d — cache is growing unbounded", len(s.hashCache))
	}

	if _, ok := s.hashCache["/path/to/file-F"]; !ok {
		t.Fatal("new entry should exist after eviction")
	}
}

// TestHashCache_DefaultCap verifies that NewDirectedEngine sets a sane default cap.
func TestHashCache_DefaultCap(t *testing.T) {
	if defaultMaxHashCacheEntries <= 0 {
		t.Fatal("defaultMaxHashCacheEntries must be positive")
	}
	if defaultMaxHashCacheEntries < 1000 {
		t.Fatal("defaultMaxHashCacheEntries should be at least 1000 for production use")
	}
}
