package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Regression/coverage for the LogSink / EventSink default adapters (A17/A18,
// core/ext_points.go). These adapters expose the JSONL Logger and
// GlobalEventLogger through the documented extension-point interfaces.
// Added 2026-10-24 (deletion-review-log).

func TestLogSink_WriteThenReadThenClosed(t *testing.T) {
	logger := mustNewLogger(t, mustTempDir(t, "logsink"), nil)
	sink := NewLogSink(logger)
	ctx := context.Background()

	ev := LogEvent{
		Event:  EventStepStarted,
		TaskID: "s1",
		StepID: "st1",
		Detail: map[string]any{"k": "v"},
		TS:     1.0,
	}
	if err := sink.Write(ctx, ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The logger writes asynchronously via a background goroutine — poll briefly.
	var got []map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := sink.ReadTaskLogs(ctx, "s1", 50)
		if err != nil {
			t.Fatalf("ReadTaskLogs: %v", err)
		}
		if len(logs) > 0 {
			got = logs
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("event not persisted within timeout")
	}
	if got[0]["event"] != string(EventStepStarted) {
		t.Errorf("event = %v, want %v", got[0]["event"], EventStepStarted)
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sink.Write(ctx, ev); !errors.Is(err, ErrLogSinkClosed) {
		t.Errorf("Write after Close = %v, want ErrLogSinkClosed", err)
	}
}

func TestLogSink_NilLogger(t *testing.T) {
	sink := NewLogSink(nil)
	ctx := context.Background()
	if err := sink.Write(ctx, LogEvent{TaskID: "x"}); !errors.Is(err, ErrLogSinkClosed) {
		t.Errorf("Write on nil logger = %v, want ErrLogSinkClosed", err)
	}
	if _, err := sink.ReadTaskLogs(ctx, "x", 0); !errors.Is(err, ErrLogSinkClosed) {
		t.Errorf("ReadTaskLogs on nil logger = %v, want ErrLogSinkClosed", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("Close on nil logger = %v, want nil", err)
	}
}

func TestEventSink_LogThenClose(t *testing.T) {
	gel, err := NewGlobalEventLogger(mustTempDir(t, "eventsink"))
	if err != nil {
		t.Fatalf("NewGlobalEventLogger: %v", err)
	}
	sink := NewEventSink(gel)
	ctx := context.Background()

	if err := sink.Log(ctx, NewAgentEvent("t1", "s1", "test_event", map[string]any{"n": 1})); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sink.Log(ctx, NewAgentEvent("t1", "", "after_close", nil)); err == nil {
		t.Error("Log after Close = nil, want error")
	}
}
