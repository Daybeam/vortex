package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// TestC14_PersistRunsAfterCallerCancel is a regression test for audit C-14:
// the async persist goroutine in RecordTaskCompletion must use a detached
// context (context.Background() with 30s timeout) instead of the caller's
// context, because the caller's context may be cancelled before persist runs,
// causing silent experience data loss.
//
// This test calls the REAL RecordTaskCompletion with a pre-cancelled context
// and verifies that PersistAll still writes files to disk.
//
// Reproduction: cancel the caller's context before calling RecordTaskCompletion,
// then check that persist files appear on disk.
func TestC14_PersistRunsAfterCallerCancel(t *testing.T) {
	dir := t.TempDir()

	es, err := NewExperienceStore(dir, nil, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore failed: %v", err)
	}

	// Pre-cancel the caller's context — simulates an HTTP request that has
	// already finished by the time the persist goroutine starts.
	callerCtx, callerCancel := context.WithCancel(context.Background())
	callerCancel()

	if callerCtx.Err() == nil {
		t.Fatal("caller context should be cancelled")
	}

	// Call RecordTaskCompletion with the cancelled context.
	records := []StepRecord{
		{
			StepID:     "s1",
			RoleID:     "engineer",
			Task:       "test task",
			Capability: "coding",
			Confidence: 0.9,
			Status:     "ok",
			Timestamp:  time.Now(),
		},
	}
	err = es.RecordTaskCompletion(callerCtx, "task-c14-test", records, 0.9, nil, true, false)
	if err != nil {
		t.Fatalf("RecordTaskCompletion failed: %v", err)
	}

	// The persist goroutine uses context.Background() (audit C-14), so it
	// should still write files to disk despite the caller's context being
	// cancelled. Poll for the file with a timeout.
	persistFile := filepath.Join(dir, "task_patterns.json")
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout: persist did not run after caller cancel — C-14 fix not working (data loss)")
		default:
		}
		if _, err := os.Stat(persistFile); err == nil {
			break // file exists — persist ran successfully
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify the file is non-empty (persist wrote actual data).
	// audit T-3.1: check os.Stat error before calling info.Size() to
	// prevent nil-pointer dereference if Stat fails.
	info, err := os.Stat(persistFile)
	if err != nil {
		t.Fatalf("stat persist file %q: %v", persistFile, err)
	}
	if info.Size() == 0 {
		t.Error("persist file is empty — PersistAll may have failed silently")
	}

	// Wait for the persist goroutine to finish so TempDir cleanup doesn't race
	// with file writes on Windows.
	waitDeadline := time.After(3 * time.Second)
	for es.persistInFlight.Load() {
		select {
		case <-waitDeadline:
			return // don't block the test forever
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestC14_PersistSkippedWhenInFlight verifies the debounce: if a persist is
// already in flight, a second RecordTaskCompletion skips the persist goroutine.
// This prevents unbounded goroutine spawn under burst task completion.
func TestC14_PersistSkippedWhenInFlight(t *testing.T) {
	dir := t.TempDir()

	es, err := NewExperienceStore(dir, nil, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore failed: %v", err)
	}

	// Manually set persistInFlight to true to simulate a persist in progress.
	es.persistInFlight.Store(true)

	ctx := context.Background()
	records := []StepRecord{
		{
			StepID:     "s1",
			RoleID:     "engineer",
			Task:       "test task",
			Capability: "coding",
			Confidence: 0.9,
			Status:     "ok",
			Timestamp:  time.Now(),
		},
	}
	err = es.RecordTaskCompletion(ctx, "task-debounce-test", records, 0.9, nil, true, false)
	if err != nil {
		t.Fatalf("RecordTaskCompletion failed: %v", err)
	}

	// Since persistInFlight was true, no persist goroutine should have been
	// spawned. The persist file should NOT exist (it was skipped).
	persistFile := filepath.Join(dir, "task_patterns.json")
	time.Sleep(100 * time.Millisecond) // give any goroutine time to run
	if _, err := os.Stat(persistFile); err == nil {
		t.Error("persist file should not exist — debounce should have skipped the persist")
	}

	// Clean up: reset persistInFlight.
	es.persistInFlight.Store(false)
}
