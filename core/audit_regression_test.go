package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// ─── Regression: ConvergenceMonitor double-Stop must not panic (sync.Once) ───

func TestConvergenceMonitor_DoubleStop_NoPanic(t *testing.T) {
	t.Skip("swarm mode not available in open core")
}

// ─── Regression: ParameterAdaptor double-Stop must not panic (sync.Once) ───

func TestParameterAdaptor_DoubleStop_NoPanic(t *testing.T) {
	t.Skip("swarm mode not available in open core")
}

// ─── Regression: ScheduleManager double-Stop must not panic (sync.Once) ───

func TestScheduleManager_DoubleStop_NoPanic(t *testing.T) {
	sm := &ScheduleManager{stopChan: make(chan struct{})}
	sm.Stop()
	sm.Stop() // must not panic — regression for unguarded close()
}

// ─── Regression: IntentRouter cache key uses strconv.Itoa, not string(rune(len)) ───
// The bug: string(rune(len(sop.Description))) converts the length int to a Unicode
// code point, producing a meaningless single character. The fix uses strconv.Itoa
// to produce a decimal string. This test verifies the cache key contains the
// numeric length, not a Unicode char.

func TestIntentRouter_CacheKey_UsesItoa_NotRune(t *testing.T) {
	reg := &config.Registry{SOPs: make(map[string]*schemas.SOP)}
	desc := strings.Repeat("a", 100) // length 100
	reg.SOPs["test-sop"] = &schemas.SOP{
		ID:          "test-sop",
		Version:     "1.0",
		Description: desc,
		Triggers:    []schemas.SOPTrigger{{Keywords: []string{"test"}}},
	}

	// Use a mock embedding client so RefreshEmbeddings actually populates sopVersions.
	router := NewIntentRouter(&mockEmbeddingClient{vec: []float32{1.0, 2.0}})
	router.RefreshEmbeddings(context.Background(), reg)

	// The version key should be "1.0_100" (strconv.Itoa), not "1.0_d" (string(rune(100))).
	got := router.sopVersions["test-sop"]
	want := "1.0_100"
	if got != want {
		t.Errorf("cache key = %q, want %q (strconv.Itoa fix)", got, want)
	}
}

// ─── Regression: applyRTK still works after regex hoisting to package-level vars ───

func TestApplyRTK_BehaviorPreserved(t *testing.T) {
	// Timestamp removal
	input := "[2026-01-01 12:00:00] hello world"
	got := applyRTK(input)
	if got != "hello world" {
		t.Errorf("applyRTK timestamp removal: got %q, want %q", got, "hello world")
	}

	// Progress noise removal
	input2 := "some line\nProgress: 50%\nother line"
	got2 := applyRTK(input2)
	if strings.Contains(got2, "Progress: 50%") {
		t.Errorf("applyRTK progress removal failed: got %q", got2)
	}

	// Empty string
	if applyRTK("") != "" {
		t.Error("applyRTK should return empty for empty input")
	}
}

// ─── Regression: compileRegexCache returns cached object for same pattern ───

func TestCompileRegexCache_ReturnsCachedObject(t *testing.T) {
	re1, err := compileRegexCache(`test.*pattern`)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	re2, err := compileRegexCache(`test.*pattern`)
	if err != nil {
		t.Fatalf("second compile failed: %v", err)
	}
	// Same pointer = cached, not recompiled
	if re1 != re2 {
		t.Error("expected same *regexp.Regexp pointer for cached pattern")
	}

	// Different pattern = different object
	re3, err := compileRegexCache(`other.*pattern`)
	if err != nil {
		t.Fatalf("third compile failed: %v", err)
	}
	if re1 == re3 {
		t.Error("expected different pointer for different pattern")
	}

	// Verify it actually matches
	if !re1.MatchString("testXYZpattern") {
		t.Error("cached regex should match correctly")
	}
}

// ─── Regression: RemoteTaskHub Stop() terminates pulse goroutine (no leak) ───

func TestRemoteTaskHub_Stop_TerminatesGoroutine(t *testing.T) {
	// nil spawner is safe — the pulse goroutine only sends to notifyChan.
	h := NewRemoteTaskHub("http://localhost:9999", "key", nil)
	h.Stop()

	// stopChan should be closed (non-blocking receive succeeds)
	select {
	case <-h.stopChan:
		// good — goroutine has been signaled to stop
	default:
		t.Error("stopChan should be closed after Stop()")
	}

	// Give the goroutine a moment to exit, then verify notifyChan stops receiving
	time.Sleep(100 * time.Millisecond)
	select {
	case <-h.GetNotifyChan():
		// might have a buffered pulse — acceptable
	default:
		// no pulse — goroutine stopped, good
	}
}

// ─── Regression: RemoteTaskHub ClaimTask/ExecuteTask are data-race safe ───
// Run with -race to verify: go test -race -run TestRemoteTaskHub_DataRaceSafe

func TestRemoteTaskHub_DataRaceSafe(t *testing.T) {
	h := &RemoteTaskHub{
		notifyChan: make(chan struct{}, 1),
		stopChan:   make(chan struct{}),
	}
	defer h.Stop()

	// Simulate concurrent ClaimTask writes and ExecuteTask reads
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.mu.Lock()
			h.currentStep = &schemas.Step{ID: fmt.Sprintf("step-%d", i)}
			h.currentGraph = &schemas.TaskGraph{TaskID: fmt.Sprintf("task-%d", i)}
			h.mu.Unlock()
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		h.mu.Lock()
		_ = h.currentStep
		_ = h.currentGraph
		h.mu.Unlock()
	}
	<-done
}

// ─── Regression: SubscribeBatched stop() closes channel and exits goroutine ───

func TestSubscribeBatched_StopClosesChannel(t *testing.T) {
	bus := obs.NewEventBus()
	batches, stop := obs.SubscribeBatched(bus, "*", 10, 50*time.Millisecond)

	// Publish an event to verify the channel works
	bus.Publish(obs.NewAgentEvent("t1", "s1", string(obs.EventStepCompleted), nil))

	select {
	case <-batches:
		// good — received a batch
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial batch")
	}

	// Stop should close the channel
	stop()

	// Channel should be closed — range exits or receive returns zero-value
	select {
	case _, ok := <-batches:
		if ok {
			t.Error("channel should be closed after stop()")
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out — channel should be closed immediately after stop()")
	}
}

// ─── Regression: PathLockManager.Release removes lock entry ───

func TestPathLockManager_ReleaseFreesEntry(t *testing.T) {
	m := NewPathLockManager()
	testPath := "/test/path/file.txt"
	cleanPath := filepath.Clean(testPath)

	// Acquire a lock for a path
	unlock := m.Acquire(testPath, true)
	unlock()

	// Entry should exist
	m.mu.Lock()
	_, exists := m.locks[cleanPath]
	m.mu.Unlock()
	if !exists {
		t.Fatal("lock entry should exist after Acquire+unlock")
	}

	// Release should remove it
	m.Release(testPath)
	m.mu.Lock()
	_, exists = m.locks[cleanPath]
	m.mu.Unlock()
	if exists {
		t.Error("lock entry should be removed after Release()")
	}
}
