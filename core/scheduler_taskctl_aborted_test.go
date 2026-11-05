package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression for O32: `/forget task <id>` deleted outputs/<id>/ — which holds
// manifest.json, the only resume source — but never touched the `tasks` row, so
// the ledger reported a human-discarded task as `running` forever and the
// compactor's terminal set could never reclaim it.

type statusWrite struct {
	taskID string
	status string
}

// abortedStatusRegistry records UpdateTaskStatus calls. The embedded nil
// interface makes any other registry method panic rather than return zeros.
type abortedStatusRegistry struct {
	store.TaskRegistry
	mu     sync.Mutex
	writes []statusWrite
}

func (r *abortedStatusRegistry) UpdateTaskStatus(_ context.Context, taskID, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, statusWrite{taskID: taskID, status: status})
	return nil
}

func (r *abortedStatusRegistry) last() (statusWrite, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.writes) == 0 {
		return statusWrite{}, 0
	}
	return r.writes[len(r.writes)-1], len(r.writes)
}

// TestMarkTaskAborted_WritesTerminalStatusLedgerOnly pins the fix: the row goes
// terminal and the in-memory graph keeps its own status, because 'aborted' is a
// ledger-only value with no GraphStatus counterpart.
func TestMarkTaskAborted_WritesTerminalStatusLedgerOnly(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	reg := &abortedStatusRegistry{}
	s.SetTaskRegistry(reg)

	graph := &schemas.TaskGraph{TaskID: "taskA", Status: schemas.GraphRunning}
	s.RegisterGraphForTest(graph)

	if err := s.MarkTaskAborted("taskA"); err != nil {
		t.Fatalf("MarkTaskAborted: %v", err)
	}

	write, n := reg.last()
	if n != 1 {
		t.Fatalf("expected exactly 1 ledger write, got %d", n)
	}
	if write.taskID != "taskA" || write.status != "aborted" {
		t.Errorf("expected (taskA, aborted), got (%q, %q)", write.taskID, write.status)
	}
	if got := s.graphs["taskA"].Status; got != schemas.GraphRunning {
		t.Errorf("in-memory graph must stay running (ledger-only transition), got %q", got)
	}
}

// TestMarkTaskAborted_FailsLoudlyWithoutRegistry covers the two ways the call
// can be silently lost: no registry configured, and an empty task ID (which
// would otherwise UPDATE every row matching ” in backends that treat it loose).
func TestMarkTaskAborted_FailsLoudlyWithoutRegistry(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()
	s.SetTaskRegistry(nil)

	err := s.MarkTaskAborted("taskA")
	if err == nil || !strings.Contains(err.Error(), "no task registry") {
		t.Errorf("expected a no-registry error, got %v", err)
	}

	reg := &abortedStatusRegistry{}
	s.SetTaskRegistry(reg)
	if err := s.MarkTaskAborted(""); err == nil {
		t.Error("expected an error for an empty task ID")
	}
	if _, n := reg.last(); n != 0 {
		t.Errorf("empty task ID must not reach the registry, got %d writes", n)
	}
}
