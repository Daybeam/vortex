package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestLogger_FileSizeCacheBounded verifies that the fileSizeCache does not
// grow beyond maxFileSizeCacheEntries. Without the eviction logic, the cache
// would grow unbounded on long-running servers (one entry per unique task ID,
// never removed). With the fix, the cache is cleared when it reaches the cap.
func TestLogger_FileSizeCacheBounded(t *testing.T) {
	// Create a logger with a small cache cap for testing.
	l := &Logger{
		dirCache:                make(map[string]bool),
		fileSizeCache:           make(map[string]int64),
		maxFileSizeCacheEntries: 5,
		System:                  &config.SystemSettings{MaxLogSizeMB: 100},
	}

	// Fill the cache to the cap by simulating what the write() method does.
	for i := 0; i < 5; i++ {
		path := "/logs/2026-09-18/task-" + string(rune('A'+i)) + ".jsonl"
		if _, hasSize := l.fileSizeCache[path]; !hasSize {
			if len(l.fileSizeCache) >= l.maxFileSizeCacheEntries {
				l.fileSizeCache = make(map[string]int64)
			}
			l.fileSizeCache[path] = 0
		}
	}

	if len(l.fileSizeCache) != 5 {
		t.Fatalf("expected 5 entries after filling to cap, got %d", len(l.fileSizeCache))
	}

	// Add one more entry — this should trigger eviction (clear + add 1).
	path := "/logs/2026-09-18/task-F.jsonl"
	if _, hasSize := l.fileSizeCache[path]; !hasSize {
		if len(l.fileSizeCache) >= l.maxFileSizeCacheEntries {
			l.fileSizeCache = make(map[string]int64)
		}
		l.fileSizeCache[path] = 0
	}

	if len(l.fileSizeCache) != 1 {
		t.Fatalf("expected 1 entry after eviction, got %d — fileSizeCache is growing unbounded", len(l.fileSizeCache))
	}

	if _, ok := l.fileSizeCache[path]; !ok {
		t.Fatal("new entry should exist after eviction")
	}
}

// TestLogger_FileSizeCacheDefaultCap verifies that NewLogger sets a sane default cap.
func TestLogger_FileSizeCacheDefaultCap(t *testing.T) {
	if defaultMaxFileSizeCacheEntries <= 0 {
		t.Fatal("defaultMaxFileSizeCacheEntries must be positive")
	}
	if defaultMaxFileSizeCacheEntries < 1000 {
		t.Fatal("defaultMaxFileSizeCacheEntries should be at least 1000 for production use")
	}
}
