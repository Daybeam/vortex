package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReplayer_LoadHistory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "replay_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logPath := filepath.Join(tmpDir, "global_trajectory.jsonl")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	events := []AgentEvent{
		NewAgentEvent("task-1", "step-1", "step_start", nil),
		NewAgentEvent("task-1", "step-1", "tool_call", map[string]any{"tool": "ls"}),
		NewAgentEvent("task-2", "step-A", "step_start", nil),
		NewAgentEvent("task-1", "step-1", "tool_result", map[string]any{"status": "ok"}),
	}

	for _, ev := range events {
		data, _ := json.Marshal(ev)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	replayer := NewReplayer(logPath)
	history, err := replayer.LoadHistory("task-1")
	if err != nil {
		t.Fatalf("failed to load history: %v", err)
	}

	if len(history) != 3 {
		t.Errorf("expected 3 events for task-1, got %d", len(history))
	}

	if history[1].EventType != "tool_call" {
		t.Errorf("expected second event to be tool_call, got %s", history[1].EventType)
	}
}

func TestReplayer_Fork(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "replay_fork_test")
	defer os.RemoveAll(tmpDir)
	logPath := filepath.Join(tmpDir, "global_trajectory.jsonl")
	f, _ := os.Create(logPath)

	events := []AgentEvent{
		{TaskID: "T", StepID: "S1", EventType: "step_start"},
		{TaskID: "T", StepID: "S1", EventType: "tool_call"},
		{TaskID: "T", StepID: "S1", EventType: "tool_result"},
		{TaskID: "T", StepID: "S2", EventType: "step_start"},
		{TaskID: "T", StepID: "S2", EventType: "tool_call"},
	}
	for _, ev := range events {
		data, _ := json.Marshal(ev)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	replayer := NewReplayer(logPath)
	forked, err := replayer.ForkTask("T", "S1")
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	if len(forked) != 3 {
		t.Errorf("expected 3 events in fork of S1, got %d", len(forked))
	}
	if forked[len(forked)-1].StepID != "S1" {
		t.Errorf("last event in fork should be S1, got %s", forked[len(forked)-1].StepID)
	}
}
