package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Replayer handles log-based state reconstruction for auditing and forking.
type Replayer struct {
	LogPath string
}

// NewReplayer creates a replayer pointed at a specific global trajectory log.
func NewReplayer(logPath string) *Replayer {
	return &Replayer{LogPath: logPath}
}

// LoadHistory retrieves all events for a specific task from the log.
func (r *Replayer) LoadHistory(taskID string) ([]AgentEvent, error) {
	f, err := os.Open(r.LogPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var history []AgentEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev AgentEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.TaskID == taskID {
			history = append(history, ev)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return history, nil
}

// ForkTask filters events up to a specific step and prepares them for a new task execution.
// This is useful for counterfactual testing: "What if at step X we used a different tool?"
func (r *Replayer) ForkTask(taskID string, atStepID string) ([]AgentEvent, error) {
	history, err := r.LoadHistory(taskID)
	if err != nil {
		return nil, err
	}

	var forkHistory []AgentEvent
	found := false
	for _, ev := range history {
		forkHistory = append(forkHistory, ev)
		if ev.StepID == atStepID {
			found = true
			// We stop after the specified step is reached.
			// Any tool_result for this step will be included if it exists.
			if ev.EventType == "tool_result" || ev.EventType == "step_completed" {
				break
			}
		}
	}

	if !found && atStepID != "" {
		return nil, fmt.Errorf("step %s not found in history of task %s", atStepID, taskID)
	}

	return forkHistory, nil
}
