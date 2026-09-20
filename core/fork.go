package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/daybeam/vortex/schemas"
)

// ForkTask creates a new task by forking from a historical step checkpoint.
// The new task inherits all completed steps up to and including targetStepID,
// and all downstream steps are reset to Pending for re-execution.
// The workspace is physically copied so the forked task has its own files.
//
// Design ref: docs/architecture/TIME_TRAVEL_AND_OBSERVABILITY_DESIGN.md
func (s *DirectedEngine) ForkTask(sourceTaskID, targetStepID string) (string, error) {
	s.Mu.RLock()
	sourceGraph, ok := s.graphs[sourceTaskID]
	s.Mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("source task %q not found", sourceTaskID)
	}

	targetStep, exists := sourceGraph.Steps[targetStepID]
	if !exists {
		return "", fmt.Errorf("step %q not found in task %q", targetStepID, sourceTaskID)
	}

	// Deep copy the graph via JSON round-trip
	data, err := json.Marshal(sourceGraph)
	if err != nil {
		return "", fmt.Errorf("failed to marshal source graph: %w", err)
	}
	var forked schemas.TaskGraph
	if err := json.Unmarshal(data, &forked); err != nil {
		return "", fmt.Errorf("failed to unmarshal forked graph: %w", err)
	}

	newTaskID := schemas.NewTaskID()
	forked.TaskID = newTaskID
	forked.ParentTaskID = sourceTaskID
	forked.ForkedAtStep = targetStepID
	forked.Status = schemas.GraphRunning
	forked.CreatedAt = targetStep.LastWorkedOn
	forked.CompletedAt = nil
	forked.PendingDecisions = []*schemas.Decision{}

	// Mark target step as OK, reset all downstream steps to Pending
	for stepID, step := range forked.Steps {
		if stepID == targetStepID {
			step.Status = schemas.StepOK
			continue
		}
		// Check if this step depends on targetStepID (directly or transitively)
		if isDownstream(stepID, targetStepID, forked.Steps) {
			step.Status = schemas.StepPending
			step.RetryCount = 0
			step.LastError = ""
			step.AutoRefineCount = 0
		}
	}

	// Copy workspace directory
	srcDir := filepath.Join(s.outputBase, sourceTaskID)
	dstDir := filepath.Join(s.outputBase, newTaskID)
	if err := copyDir(srcDir, dstDir); err != nil {
		return "", fmt.Errorf("failed to copy workspace: %w", err)
	}

	// Inject into scheduler
	s.Mu.Lock()
	s.graphs[newTaskID] = &forked
	ctx, cancel := context.WithCancel(s.lifecycleCtx)
	s.cancelFuncs[newTaskID] = cancel
	s.doneChans[newTaskID] = make(chan struct{})
	s.Mu.Unlock()

	s.persistGraph(&forked)
	s.Broadcast()

	s.logger.Log("EventTaskForked", newTaskID, targetStepID, map[string]any{
		"parent_task": sourceTaskID,
		"forked_at":   targetStepID,
	})

	// Start execution
	s.goBackground(func() { s.run(ctx, newTaskID) }) // audit M8: use goBackground so Stop() drains

	return newTaskID, nil
}

// isDownstream checks if stepID is a direct or transitive dependent of targetStepID.
func isDownstream(stepID, targetStepID string, steps map[string]*schemas.Step) bool {
	visited := make(map[string]bool)
	var check func(id string) bool
	check = func(id string) bool {
		if visited[id] {
			return false
		}
		visited[id] = true
		step, ok := steps[id]
		if !ok {
			return false
		}
		for _, dep := range step.DependsOn {
			if dep == targetStepID {
				return true
			}
			if check(dep) {
				return true
			}
		}
		return false
	}
	return check(stepID)
}

// copyDir recursively copies src to dst, skipping errors for missing source.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
