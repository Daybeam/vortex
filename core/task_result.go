package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// ResolveTaskResult returns the best available terminal answer for a task.
// ACP uses it when the task-completed event has no inline result payload.
func (s *DirectedEngine) ResolveTaskResult(taskID string) string {
	if s == nil || s.taskStore == nil {
		return ""
	}

	stepIDs := s.terminalStepIDs(taskID)
	if len(stepIDs) == 0 {
		stepIDs = s.successfulStepIDs(taskID)
	}
	if len(stepIDs) == 0 {
		return ""
	}

	ctx := s.lifecycleCtx
	if ctx == nil {
		ctx = context.Background()
	}
	for i := len(stepIDs) - 1; i >= 0; i-- {
		result, err := s.taskStore.Get(ctx, taskID, stepIDs[i])
		if err != nil || result == nil {
			continue
		}
		if text := taskResultText(result.Data); text != "" {
			return truncateTaskResult(text)
		}
	}
	return ""
}

func (s *DirectedEngine) terminalStepIDs(taskID string) []string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	graph := s.graphs[taskID]
	if graph == nil {
		return nil
	}

	dependedOn := make(map[string]bool)
	for _, step := range graph.Steps {
		for _, dep := range step.DependsOn {
			dependedOn[dep] = true
		}
	}
	ids := make([]string, 0, len(graph.Steps))
	for id, step := range graph.Steps {
		if step.Status == schemas.StepOK && !dependedOn[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *DirectedEngine) successfulStepIDs(taskID string) []string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	graph := s.graphs[taskID]
	if graph == nil {
		return nil
	}
	ids := make([]string, 0, len(graph.Steps))
	for id, step := range graph.Steps {
		if step.Status == schemas.StepOK {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func taskResultText(data any) string {
	switch value := data.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	case map[string]any:
		for _, key := range []string{"raw", "text", "content", "answer", "summary", "result"} {
			if text, ok := value[key].(string); ok && text != "" {
				return text
			}
		}
		for _, key := range []string{"result", "answer"} {
			if nested, ok := value[key].(map[string]any); ok {
				if text := taskResultText(nested); text != "" {
					return text
				}
			}
		}
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
		return fmt.Sprint(value)
	default:
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
		return fmt.Sprint(value)
	}
}

func truncateTaskResult(text string) string {
	const maxResultLen = 10000
	if len(text) <= maxResultLen {
		return text
	}
	return strings.TrimSpace(text[:maxResultLen]) + "...[truncated]"
}
