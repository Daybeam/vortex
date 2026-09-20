package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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
