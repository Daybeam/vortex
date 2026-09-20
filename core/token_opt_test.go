package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

func TestTaskGraph_ToStatusDictView(t *testing.T) {
	graph := &schemas.TaskGraph{
		TaskID: "task_1",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:     "s1",
				Task:   "Step 1 Task",
				Status: schemas.StepOK,
			},
			"s2": {
				ID:        "s2",
				Task:      "Step 2 Task",
				Status:    schemas.StepRunning,
				LastError: "Some error in s2",
			},
		},
		CreatedAt: time.Now(),
	}

	t.Run("SummaryView", func(t *testing.T) {
		status := graph.ToStatusDictView("summary")
		if status["task_id"] != "task_1" {
			t.Errorf("Expected task_id task_1, got %v", status["task_id"])
		}
		if status["progress"] != "1/2" {
			t.Errorf("Expected progress 1/2, got %v", status["progress"])
		}
		if status["active_step_id"] != "s2" {
			t.Errorf("Expected active_step_id s2, got %v", status["active_step_id"])
		}
		if status["last_error"] != "Some error in s2" {
			t.Errorf("Expected last_error, got %v", status["last_error"])
		}
		if _, ok := status["steps"]; ok {
			t.Error("Summary view should not contain 'steps' detail")
		}
	})

	t.Run("FullView", func(t *testing.T) {
		status := graph.ToStatusDictView("full")
		if _, ok := status["steps"]; !ok {
			t.Error("Full view should contain 'steps' detail")
		}
		steps := status["steps"].(map[string]any)
		if len(steps) != 2 {
			t.Errorf("Expected 2 steps in full view, got %d", len(steps))
		}
	})
}

func TestLogger_ReadTaskLogsPagination(t *testing.T) {
	tmpDir := t.TempDir()
	logger, _ := NewLogger(tmpDir, nil)
	defer logger.Close()

	taskID := "task_pagination"
	for i := 1; i <= 10; i++ {
		logger.Log("test_event", taskID, fmt.Sprintf("s%d", i), map[string]any{"index": i})
	}

	// Wait for logs to be written
	time.Sleep(200 * time.Millisecond)

	t.Run("LimitOnly", func(t *testing.T) {
		events, err := logger.ReadTaskLogs(taskID, 5, 0)
		if err != nil {
			t.Fatalf("ReadTaskLogs failed: %v", err)
		}
		if len(events) != 5 {
			t.Errorf("Expected 5 events, got %d", len(events))
		}
		if events[0]["step_id"] != "s1" {
			t.Errorf("Expected first event to be s1, got %v", events[0]["step_id"])
		}
	})

	t.Run("OffsetAndLimit", func(t *testing.T) {
		events, err := logger.ReadTaskLogs(taskID, 3, 5)
		if err != nil {
			t.Fatalf("ReadTaskLogs failed: %v", err)
		}
		if len(events) != 3 {
			t.Errorf("Expected 3 events, got %d", len(events))
		}
		if events[0]["step_id"] != "s6" {
			t.Errorf("Expected first event to be s6 (offset 5), got %v", events[0]["step_id"])
		}
	})

	t.Run("OffsetOutOfBounds", func(t *testing.T) {
		events, err := logger.ReadTaskLogs(taskID, 5, 20)
		if err != nil {
			t.Fatalf("ReadTaskLogs failed: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("Expected 0 events, got %d", len(events))
		}
	})
}
