package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGlobalEventLogger(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "event_log_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logger, err := NewGlobalEventLogger(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	event := NewAgentEvent("task-1", "step-1", "test_event", map[string]any{"data": 123})
	if err := logger.Log(event); err != nil {
		t.Errorf("failed to log event: %v", err)
	}

	// Wait a bit for the background writer to catch up
	time.Sleep(100 * time.Millisecond)

	logPath := filepath.Join(tmpDir, "global_trajectory.jsonl")
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		t.Errorf("expected at least one line in log file")
	}

	var loggedEvent AgentEvent
	if err := json.Unmarshal(scanner.Bytes(), &loggedEvent); err != nil {
		t.Fatalf("failed to unmarshal logged event: %v", err)
	}

	if loggedEvent.EventID != event.EventID {
		t.Errorf("expected event ID %s, got %s", event.EventID, loggedEvent.EventID)
	}
	if loggedEvent.EventType != "test_event" {
		t.Errorf("expected event type test_event, got %s", loggedEvent.EventType)
	}
}

func TestLogger_HookEventBus(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "event_bus_hook_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logger, err := NewGlobalEventLogger(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	bus := NewEventBus()
	logger.HookEventBus(bus)

	event := NewAgentEvent("task-2", "step-2", "hooked_event", nil)
	bus.Publish(event)

	time.Sleep(100 * time.Millisecond)

	logPath := filepath.Join(tmpDir, "global_trajectory.jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("expected data in log file, got empty")
	}
}

// TestGlobalEventLogger_ConcurrentLogCloseNoPanic is a regression test for
// audit fix A (TOCTOU panic): concurrent Log + Close must never panic with
// "send on closed channel". Before the fix, the atomic-flag-only guard left a
// window where Log read closed=0, Close set closed + close(ch), then Log sent
// on the closed channel → panic. Run with -race -count=50 to stress.
func TestGlobalEventLogger_ConcurrentLogCloseNoPanic(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "event_log_toctou")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logger, err := NewGlobalEventLogger(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	ev := NewAgentEvent("toctou", "s", "e", nil)
	var wg sync.WaitGroup
	const loggers = 16
	wg.Add(loggers)
	for i := 0; i < loggers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				// Log returns an error once closed; that is expected and safe.
				// A panic here (send on closed channel) fails the test process.
				_ = logger.Log(ev)
			}
		}()
	}
	// Close while loggers are still hammering Log — the race window.
	logger.Close()
	wg.Wait()
	// Reaching here means no goroutine panicked on send-to-closed-channel.
}
