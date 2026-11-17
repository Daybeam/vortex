package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
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
// including those without pending decisions. It needs both halves to do that:
// failStaleTasks for graphs this process holds, sweepStaleTaskRows for ledger
// rows left behind by a process that died.
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
// Rows this process does not hold are swept afterwards by sweepStaleTaskRows.
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
			"reason":        "stale_task_auto_fail",
			"timeout_hours": int(timeout.Hours()),
			"auto_failed":   true,
		})
		fmt.Fprintf(os.Stderr, "[stale-task-monitor] Auto-failed task %s: no progress for %s\n",
			taskID, timeout)
	}

	s.sweepStaleTaskRows(timeout)
}

// sweepStaleTaskRows closes ledger rows the loop above cannot see. failStaleTasks
// walks s.graphs, and nothing rehydrates a dead process's rows into that map, so
// a row left 'running' by a crash or a restart is invisible to it: measured on
// task_eb209a68, 'running' since 2026-11-06 12:12 with the frontend still polling
// it four days later (F10, docs/completed/2026-11-15/TELEGRAM_CALLBACK_AND_VCP_FINDINGS_2026-11-10.md).
//
// The cutoff is the row's own updated_at rather than a first-sight timestamp, so
// that row closes on the next tick instead of waiting another full timeout.
// Multi-hub safety is the store's guarded UPDATE, not anything here: a hub that
// is alive and driving the task keeps bumping updated_at, so the row never
// qualifies, and one that writes between our SELECT and UPDATE makes it match no
// rows. See store.StaleTaskSweeper.
func (s *DirectedEngine) sweepStaleTaskRows(timeout time.Duration) {
	sweeper, ok := s.taskRegistry().(store.StaleTaskSweeper)
	if !ok {
		return // no ledger, or one without the capability (file mode, mocks)
	}
	closed, err := sweeper.SweepStaleRunning(s.lifecycleCtx, time.Now().Add(-timeout))
	// Reconcile before looking at err: the store returns the rows it already
	// closed alongside a mid-sweep failure, and a closed row is never a candidate
	// again, so skipping them here would leave them diverged permanently.
	for _, taskID := range closed {
		s.reconcileSweptTask(taskID)
		s.logger.Log(obs.EventTaskFailed, taskID, "", map[string]any{
			"reason":        "stale_task_row_sweep",
			"timeout_hours": int(timeout.Hours()),
			"auto_failed":   true,
		})
		fmt.Fprintf(os.Stderr, "[stale-task-monitor] Closed orphaned ledger row %s: no write for %s\n",
			taskID, timeout)
	}
	if err != nil {
		// A busy or unreadable DB is not evidence that a task is dead. Leave the
		// remaining rows for the next tick rather than guessing.
		fmt.Fprintf(os.Stderr, "[stale-task-monitor] Ledger sweep stopped after %d rows: %v\n", len(closed), err)
	}
}

// reconcileSweptTask brings an in-memory graph in line with a row the sweep just
// closed. GetStatus answers from s.graphs first (core/scheduler_status.go:73), so
// without this the UI would keep showing 'running' until the loop above aged the
// same task out on its own first-sight clock. No persistGraph here: the sweep
// already wrote the ledger, and claiming a fence for a task we just declared dead
// would preempt whoever is still driving it.
func (s *DirectedEngine) reconcileSweptTask(taskID string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if graph := s.graphs[taskID]; graph != nil && graph.Status == schemas.GraphRunning {
		graph.Status = schemas.GraphFailed
		if cancel, ok := s.cancelFuncs[taskID]; ok {
			cancel()
		}
	}
	delete(s.staleTaskSnapshots, taskID)
}

// taskProgressHash computes a hash of the graph's progress state: graph status
// + all step statuses + retry counts. If this hash doesn't change between two
// consecutive checks, the task has made no progress.
//
// Steps are visited in sorted key order because Go randomises map iteration.
// Without the sort an unchanged multi-step graph hashes differently on most
// checks, failStaleTasks reads that as progress, resets lastProgress to now, and
// the task is never auto-failed — the older the graph, the worse it gets (a
// 4-step graph reproduces one particular order about 1 time in 24). Measured at
// HEAD before the sort: TestTaskProgressHash_StableForSameState failed 2 of 8
// runs on a 2-step graph.
func taskProgressHash(graph *schemas.TaskGraph) string {
	ids := make([]string, 0, len(graph.Steps))
	for id := range graph.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var buf []byte
	buf = append(buf, []byte(string(graph.Status))...)
	for _, id := range ids {
		step := graph.Steps[id]
		buf = append(buf, []byte(step.ID)...)
		buf = append(buf, []byte(string(step.Status))...)
		buf = append(buf, []byte(fmt.Sprintf("%d", step.RetryCount))...)
	}
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])
}
