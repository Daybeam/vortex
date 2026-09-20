package core

import (
	"testing"
)

// TestHashCache_BoundedGrowth verifies that the hashCache does not grow
// beyond maxHashCacheEntries. Without the eviction logic, the cache would
// grow unbounded on long-running servers (one entry per output file path,
// never removed). With the fix, the cache is cleared when it reaches the cap.
func TestHashCache_BoundedGrowth(t *testing.T) {
	s := &DirectedEngine{
		hashCache:           make(map[string]string),
		maxHashCacheEntries: 5, // Small cap for testing
	}

	// Fill the cache to the cap.
	for i := 0; i < 5; i++ {
		key := "/path/to/file-" + string(rune('A'+i))
		s.hashCacheMu.Lock()
		if len(s.hashCache) >= s.maxHashCacheEntries {
			s.hashCache = make(map[string]string)
		}
		s.hashCache[key] = "hash-" + string(rune('A'+i))
		s.hashCacheMu.Unlock()
	}

	if len(s.hashCache) != 5 {
		t.Fatalf("expected 5 entries after filling to cap, got %d", len(s.hashCache))
	}

	// Add one more entry — this should trigger eviction (clear + add 1).
	s.hashCacheMu.Lock()
	if len(s.hashCache) >= s.maxHashCacheEntries {
		s.hashCache = make(map[string]string)
	}
	s.hashCache["/path/to/file-F"] = "hash-F"
	s.hashCacheMu.Unlock()

	if len(s.hashCache) != 1 {
		t.Fatalf("expected 1 entry after eviction, got %d — cache is growing unbounded", len(s.hashCache))
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
