package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestLogger_FileSizeCacheBounded verifies that the fileSizeCache does not
// grow beyond maxFileSizeCacheEntries. Without the eviction logic, the cache
// would grow unbounded on long-running servers (one entry per unique task ID,
// never removed). With the P-MED-8 fix, the cache uses partial eviction
// (deletes ~25% of entries) instead of full clear to prevent cache stampede.
//
// audit T-1.2: This test now calls the production recordFileSizeCacheEntry
// method instead of inlining a copy of the eviction logic. Testing the actual
// production code ensures that changes to the eviction logic are caught.
func TestLogger_FileSizeCacheBounded(t *testing.T) {
	l := &Logger{
		dirCache:                make(map[string]bool),
		fileSizeCache:           make(map[string]int64),
		maxFileSizeCacheEntries: 5,
		System:                  &config.SystemSettings{MaxLogSizeMB: 100},
	}

	for i := 0; i < 5; i++ {
		path := "/logs/2026-09-18/task-" + string(rune('A'+i)) + ".jsonl"
		l.recordFileSizeCacheEntry(path)
	}

	if len(l.fileSizeCache) != 5 {
		t.Fatalf("expected 5 entries after filling to cap, got %d", len(l.fileSizeCache))
	}

	// Add one more entry — this triggers partial eviction (~25% deleted) + add 1.
	// With 5 entries: delete 1 (every 4th) → 4 remain → add 1 → 5 total.
	path := "/logs/2026-09-18/task-F.jsonl"
	l.recordFileSizeCacheEntry(path)

	// After partial eviction, cache should still be bounded (not growing unbounded).
	if len(l.fileSizeCache) > 5 {
		t.Fatalf("expected ≤5 entries after eviction, got %d — fileSizeCache is growing unbounded", len(l.fileSizeCache))
	}
	if len(l.fileSizeCache) < 4 {
		t.Fatalf("expected ≥4 entries after partial eviction, got %d — too aggressive", len(l.fileSizeCache))
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

// TestLogger_Log_PublishesToEventBus verifies that Logger.Log broadcasts
// events with a non-empty TaskID to DefaultBus, while skipping events
// without a TaskID (internal logging noise).
//
// This is a regression test: before the fix, Logger.Log only wrote to
// file and never published to EventBus, so webhook_notifier, chat SSE,
// and CaSKG could only see the handful of events that scheduler called
// s.publishEvent for explicitly.
func TestLogger_Log_PublishesToEventBus(t *testing.T) {
	original := DefaultBus
	defer func() { DefaultBus = original }()
	bus := NewEventBus()
	DefaultBus = bus

	logger := &Logger{
		ch:              make(chan LogEvent, 100),
		dirCache:        make(map[string]bool),
		fileSizeCache:   make(map[string]int64),
		System:          &config.SystemSettings{MaxLogSizeMB: 100},
	}

	var received []AgentEvent
	bus.Subscribe("*", func(ev AgentEvent) error {
		received = append(received, ev)
		return nil
	})

	// Event with TaskID — should be published.
	logger.Log(EventTaskCompleted, "task-1", "step-1", map[string]any{"status": "completed"})

	// Event without TaskID — should be skipped.
	logger.Log(EventTaskCompleted, "", "step-1", map[string]any{"status": "completed"})

	if len(received) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(received))
	}
	if received[0].TaskID != "task-1" {
		t.Fatalf("expected task-1, got %q", received[0].TaskID)
	}
}

// TestLogger_Log_SkipsEmptyTaskID verifies that events without a TaskID
// (internal logging) are not published to the EventBus.
//
// Regression: prevents EventBus noise from internal logging operations
// like role_missing, rate_limited, etc. that don't carry task context.
func TestLogger_Log_SkipsEmptyTaskID(t *testing.T) {
	original := DefaultBus
	defer func() { DefaultBus = original }()
	bus := NewEventBus()
	DefaultBus = bus

	logger := &Logger{
		ch:              make(chan LogEvent, 100),
		dirCache:        make(map[string]bool),
		fileSizeCache:   make(map[string]int64),
		System:          &config.SystemSettings{MaxLogSizeMB: 100},
	}

	count := 0
	bus.Subscribe("*", func(ev AgentEvent) error {
		count++
		return nil
	})

	// Multiple events without TaskID — none should be published.
	logger.Log(EventRoleMissing, "", "", map[string]any{})
	logger.Log(EventRateLimited, "", "", map[string]any{})
	logger.Log(EventTaskSubmitted, "", "", map[string]any{})

	if count != 0 {
		t.Fatalf("expected 0 published events for empty TaskID, got %d", count)
	}
}
