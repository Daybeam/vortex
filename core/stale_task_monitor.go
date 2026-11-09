package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// staleTaskState tracks the last known progress of a task. If the progressHash
// hasn't changed between checks, the task has made no progress.
type staleTaskState struct {
	progressHash string
	lastProgress time.Time
}

// defaultStaleTaskAutoFailTimeout is the operator's number: a task that has
// made no progress (no status change, no step status change, no retry count
// change) for 24 hours is stuck and should be auto-failed. This is stricter
// than the existing stale_decision_report (Q5-5), which is report-only and
// covers only pending decisions — this monitor covers ALL running tasks,
// including those without pending decisions.
const defaultStaleTaskAutoFailTimeout = 24 * time.Hour

// staleTaskAutoFailTimeout reads system.stale_task_auto_fail_hours with the
// same convention as decisionStaleTimeout: nil ⇒ default, <=0 ⇒ disabled.
func (s *DirectedEngine) staleTaskAutoFailTimeout() time.Duration {
	if s.registry == nil {
		return defaultStaleTaskAutoFailTimeout
	}
	h := s.registry.System.StaleTaskAutoFailHours
	if h == nil {
		return defaultStaleTaskAutoFailTimeout
	}
	if *h <= 0 {
		return 0
	}
	return time.Duration(*h) * time.Hour
}

// startStaleTaskMonitor launches a background goroutine that periodically
// checks all running tasks for staleness. If a task has made no progress
// (no status change, no step status change, no retry count change) for the
// configured timeout, it is auto-failed with GraphFailed status.
func (s *DirectedEngine) startStaleTaskMonitor() {
	timeout := s.staleTaskAutoFailTimeout()
	if timeout <= 0 {
		return
	}

	s.goBackground(func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-s.lifecycleCtx.Done():
				return
			case <-ticker.C:
				s.failStaleTasks(timeout)
			}
		}
	})
}

// failStaleTasks checks all running tasks and auto-fails those that have made
// no progress for the configured timeout. Progress is measured by a hash of
// the graph status and all step statuses + retry counts — if the hash hasn't
// changed between two consecutive checks, the task has made no progress.
func (s *DirectedEngine) failStaleTasks(timeout time.Duration) {
	now := time.Now()
	var toFail []string
	var toPersist []*schemas.TaskGraph

	s.Mu.Lock()
	for taskID, graph := range s.graphs {
		if graph == nil || graph.Status != schemas.GraphRunning {
			continue
		}

		currentHash := taskProgressHash(graph)
		prev, exists := s.staleTaskSnapshots[taskID]

		if exists && prev.progressHash == currentHash {
			// No progress since last check.
			if now.Sub(prev.lastProgress) >= timeout {
				graph.Status = schemas.GraphFailed
				if cancel, ok := s.cancelFuncs[taskID]; ok {
					cancel()
				}
				toFail = append(toFail, taskID)
				toPersist = append(toPersist, graph)
				delete(s.staleTaskSnapshots, taskID)
			}
		} else {
			// Progress made or first check — update snapshot.
			s.staleTaskSnapshots[taskID] = &staleTaskState{
				progressHash: currentHash,
				lastProgress: now,
			}
		}
	}
	s.Mu.Unlock()

	// Persist and log outside the lock.
	for _, graph := range toPersist {
		s.persistGraph(graph)
	}
	for _, taskID := range toFail {
		s.logger.Log(obs.EventTaskFailed, taskID, "", map[string]any{
			"reason":          "stale_task_auto_fail",
			"timeout_hours":   int(timeout.Hours()),
			"auto_failed":     true,
		})
		fmt.Fprintf(os.Stderr, "[stale-task-monitor] Auto-failed task %s: no progress for %s\n",
			taskID, timeout)
	}
}

// taskProgressHash computes a hash of the graph's progress state: graph status
// + all step statuses + retry counts. If this hash doesn't change between two
// consecutive checks, the task has made no progress.
func taskProgressHash(graph *schemas.TaskGraph) string {
	var buf []byte
	buf = append(buf, []byte(string(graph.Status))...)
	for _, step := range graph.Steps {
		buf = append(buf, []byte(step.ID)...)
		buf = append(buf, []byte(string(step.Status))...)
		buf = append(buf, []byte(fmt.Sprintf("%d", step.RetryCount))...)
	}
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])
}
