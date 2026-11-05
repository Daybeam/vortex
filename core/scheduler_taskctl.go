package core

import (
	"context"
	"fmt"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

func (s *DirectedEngine) Broadcast() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	close(s.notifyChan)
	s.notifyChan = make(chan struct{})
}

func (s *DirectedEngine) GetNotifyChan() chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyChan
}

func (s *DirectedEngine) CancelTask(taskID string) error {
	s.Mu.Lock()
	cancel, ok := s.cancelFuncs[taskID]
	graph, ok2 := s.graphs[taskID]
	if !ok || !ok2 {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found or already terminal", taskID)
	}
	cancel()
	graph.Status = schemas.GraphCancelled
	s.Mu.Unlock()
	s.persistGraph(graph) // audit PERF-HIGH-1: I/O outside lock (was persistGraphLocked)

	// L-7.3: wait for in-flight step goroutines to drain so late
	// persistGraph calls don't re-create the task after a subsequent Clear.
	s.waitStepDrain(taskID, 5*time.Second)

	// audit HIGH (2026-10-28): clear EffectTracker on cancel too.
	if s.spawner != nil {
		s.spawner.ClearEffectTracker(taskID)
	}
	// audit MEDIUM (2026-10-28): clear lastPersistAt on cancel too.
	s.lastPersistAt.Delete(taskID)
	return nil
}

// MarkTaskAborted sets the ledger row to a terminal status without touching the
// in-memory graph. 'aborted' is ledger-only (GraphStatus has no counterpart) and
// is already in the compactor's terminal set; it says "a human discarded this",
// which is why it is not 'failed'.
func (s *DirectedEngine) MarkTaskAborted(taskID string) error {
	if taskID == "" {
		return fmt.Errorf("MarkTaskAborted: task ID required")
	}
	if s.TaskRegistry == nil {
		return fmt.Errorf("MarkTaskAborted: no task registry, ledger row for %q left as-is", taskID)
	}
	return s.TaskRegistry.UpdateTaskStatus(s.lifecycleCtx, taskID, "aborted")
}

// SuspendTask pauses a running task by cancelling its execution context and
// transitioning the graph to GraphSuspended. Unlike CancelTask, the task can
// be resumed later via ResumeSuspendedTask. This is a Control Plane operation
// (human/system-facing), NOT exposed as an LLM-facing MCP tool.
func (s *DirectedEngine) SuspendTask(taskID string) error {
	s.Mu.Lock()
	cancel, ok := s.cancelFuncs[taskID]
	graph, ok2 := s.graphs[taskID]
	if !ok || !ok2 {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found or already terminal", taskID)
	}
	if graph.Status != schemas.GraphRunning {
		s.Mu.Unlock()
		return fmt.Errorf("task %q is not running (status: %s)", taskID, graph.Status)
	}
	cancel()
	graph.Status = schemas.GraphSuspended
	s.logger.Log(obs.EventTaskSuspended, taskID, "", map[string]any{})
	s.Mu.Unlock()
	s.persistGraph(graph) // audit PERF-HIGH-1: I/O outside lock (was persistGraphLocked)

	// L-7.3: wait for in-flight step goroutines to drain (same as CancelTask).
	s.waitStepDrain(taskID, 5*time.Second)
	return nil
}

// ResumeSuspendedTask resumes a previously suspended task by re-initializing
// its execution context and respawning the scheduler loop from pending steps.
// This is a Control Plane operation (human/system-facing).
func (s *DirectedEngine) ResumeSuspendedTask(taskID string) error {
	s.Mu.Lock()
	graph, ok := s.graphs[taskID]
	if !ok {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}
	if graph.Status != schemas.GraphSuspended {
		s.Mu.Unlock()
		return fmt.Errorf("task %q is not suspended (status: %s)", taskID, graph.Status)
	}

	// Reset zombie steps to pending so s.run() picks them up.
	for _, step := range graph.Steps {
		if step.Status == schemas.StepRunning {
			step.Status = schemas.StepPending
		}
	}
	graph.Status = schemas.GraphRunning

	ctx, cancel := context.WithCancel(s.lifecycleCtx)
	s.cancelFuncs[graph.TaskID] = cancel
	if _, ok := s.doneChans[graph.TaskID]; !ok {
		s.doneChans[graph.TaskID] = make(chan struct{})
	}
	// audit L-N1: startGraphExecutor handles UseSwarm branching.
	s.startGraphExecutor(ctx, graph.TaskID)
	s.logger.Log(obs.EventTaskResumed, taskID, "", map[string]any{})
	s.Mu.Unlock()
	s.goBackground(func() { s.persistGraph(graph) }) // audit PERF-HIGH-1: async I/O outside lock; was persistGraphLocked
	return nil
}

func (s *DirectedEngine) PatchWorkspace(taskID string, data map[string]any) error {
	s.Mu.Lock()
	graph, ok := s.graphs[taskID]
	if !ok {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}

	if graph.GlobalWorkspace == nil {
		graph.GlobalWorkspace = make(map[string]any)
	}

	// Merge patch data
	for k, v := range data {
		graph.GlobalWorkspace[k] = v
	}
	s.Mu.Unlock()

	// Audit Log
	s.logger.Log("EventWorkspacePatched", taskID, "", map[string]any{
		"patched_keys": func() []string {
			keys := make([]string, 0, len(data))
			for k := range data {
				keys = append(keys, k)
			}
			return keys
		}(),
		"reason": "manual_intervention",
	})

	return nil
}

func (s *DirectedEngine) recordCoordinationEdge(graph *schemas.TaskGraph, source, target, edgeType string, payload int64) {
	s.walker.RecordCoordinationEdge(graph, source, target, edgeType, payload)
}

// GetCrossFamilyVerifier returns a provider ID from a different model family
// than the given provider (identified by provider ID). Falls back to "" when
// the provider is unknown or no cross-family provider is available.
func (e *DirectedEngine) GetCrossFamilyVerifier(providerID string) string {
	return e.walker.GetCrossFamilyVerifier(providerID)
}

// RegisterGraphForTest allows unit tests to inject a TaskGraph into the engine.
func (s *DirectedEngine) RegisterGraphForTest(graph *schemas.TaskGraph) {
	if graph == nil || graph.TaskID == "" {
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.graphs[graph.TaskID] = graph
}
