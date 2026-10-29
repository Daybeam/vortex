package core

import (
	"encoding/json"
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// finalizeSmartRoutedAfterDecision handles terminal finalization for a
// smart-routed (single-step) graph after all decisions are resolved.
// Such graphs have no run() loop (startGraphExecutor is multi-step only),
// so Broadcast has no consumer — finalize inline instead, mirroring
// FulfillStep's IsSmartRouted branch (~line 390).
//
// MUST be called with s.Mu held. On success, releases the lock, sets
// *unlocked = true, persists, and publishes a terminal event. Returns
// true if finalized (caller must return immediately without re-locking).
// Returns false (lock still held) if not applicable.
func (s *DirectedEngine) finalizeSmartRoutedAfterDecision(
	graph *schemas.TaskGraph, taskID, stepID string, unlocked *bool,
) bool {
	if !graph.IsSmartRouted || len(graph.PendingDecisions) > 0 {
		return false
	}
	// Determine terminal status from step states, mirroring finalize()
	// (scheduler_decision_finalize.go:28-53).
	allSkipped := true
	anyFailed := false
	for _, st := range graph.Steps {
		if st == nil {
			continue
		}
		switch st.Status {
		case schemas.StepFailed, schemas.StepBlocked:
			anyFailed = true
		case schemas.StepSkipped:
			// still allSkipped
		default:
			allSkipped = false
		}
	}
	if anyFailed {
		if graph.Status != schemas.GraphFailed {
			graph.Status = schemas.GraphFailed
		}
	} else if allSkipped {
		graph.Status = schemas.GraphCompletedWithSkips
	} else {
		graph.Status = schemas.GraphCompleted
	}
	// Close doneChan under lock (safe — caller holds Mu).
	if ch, ok := s.doneChans[taskID]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	// Release lock for persistGraph (takes RLock — sync.RWMutex is not
	// re-entrant, so calling it under our held Lock would deadlock).
	s.Mu.Unlock()
	*unlocked = true
	s.persistGraph(graph)
	if graph.Status == schemas.GraphFailed {
		s.publishEvent(taskID, stepID, EventTaskFailed, nil)
	} else {
		s.publishEvent(taskID, stepID, EventTaskCompleted, nil)
	}
	return true
}

// applyGoalChangeLocked handles the goal_change_requested decision choice.
// The payload is a JSON TaskCharter. On approval, the charter Version is
// incremented and the graph's Charter is replaced. This makes "silently
// switching direction" into an auditable, versioned event.
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P2.
// MUST be called with s.Mu held.
func (s *DirectedEngine) applyGoalChangeLocked(graph *schemas.TaskGraph, taskID, payload string) error {
	var newCharter schemas.TaskCharter
	if err := json.Unmarshal([]byte(payload), &newCharter); err != nil {
		return fmt.Errorf("invalid charter data for goal_change_requested: %v", err)
	}
	// Increment version: start at 1 if no prior charter, else bump.
	if graph.Charter != nil {
		newCharter.Version = graph.Charter.Version + 1
	} else {
		newCharter.Version = 1
	}
	oldVersion := 0
	if graph.Charter != nil {
		oldVersion = graph.Charter.Version
	}
	graph.Charter = &newCharter
	s.logger.Log("EventGoalChangeApproved", taskID, "", map[string]any{
		"old_version": oldVersion,
		"new_version": newCharter.Version,
		"intent":      newCharter.Intent,
	})
	return nil
}
