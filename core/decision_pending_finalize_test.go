package core

import (
	"context"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestFinalize_DoesNotFinalizeWithPendingDecision verifies that finalize()
// never marks a graph as GraphCompleted when it still has pending decisions.
//
// Without the C5DD fix, finalize() derived the terminal verdict from allOK
// only, so a graph whose step had paused on a decision could be written out
// as completed. This is the root cause of the "task actually completed but
// status shows running 0/1" bug: the graph is marked completed, but the
// pending decision means the task is not truly done.
//
// The fix routes the terminal status through finalStatus and raises
// GraphBlocked when PendingDecisions is non-empty.
func TestFinalize_DoesNotFinalizeWithPendingDecision(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	s := &DirectedEngine{
		logger:          logger,
		outputBase:      t.TempDir(),
		graphs:          make(map[string]*schemas.TaskGraph),
		doneChans:       make(map[string]chan struct{}),
		bgWg:            sync.WaitGroup{},
		sieve:           NewSieve(1),
		spawner:         &Spawner{},
		notifyChan:      make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		registry: &config.Registry{
			System: config.SystemSettings{},
		},
	}

	taskID := "c5dd_pending_decision"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			// StepPartial is key: it is neither StepFailed nor StepBlocked,
			// so finalize's allOK stays true. Without the fix, the graph
			// would be marked GraphCompleted despite having a pending decision.
			"s1": {ID: "s1", RoleID: "appworld_agent", Task: "do the thing", Status: schemas.StepPartial},
		},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec_c5dd",
			StepID:  "s1",
			Type:    schemas.DecisionStepFailed,
			Options: []string{"skip", "abort", "retry", "refine_and_retry"},
		}},
	}
	s.graphs[taskID] = graph
	s.doneChans[taskID] = make(chan struct{})

	s.finalize(graph)

	if graph.Status != schemas.GraphBlocked {
		t.Errorf("expected GraphBlocked (pending decision), got %s", graph.Status)
	}
}

// TestFinalize_CompletesWithoutPendingDecision is the reverse case: a graph
// with no pending decisions and all steps non-failed should be marked
// GraphCompleted.
func TestFinalize_CompletesWithoutPendingDecision(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	s := &DirectedEngine{
		logger:          logger,
		outputBase:      t.TempDir(),
		graphs:          make(map[string]*schemas.TaskGraph),
		doneChans:       make(map[string]chan struct{}),
		bgWg:            sync.WaitGroup{},
		sieve:           NewSieve(1),
		spawner:         &Spawner{},
		notifyChan:      make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		registry: &config.Registry{
			System: config.SystemSettings{},
		},
	}

	taskID := "c5dd_no_pending"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "appworld_agent", Task: "do the thing", Status: schemas.StepOK},
		},
		PendingDecisions: []*schemas.Decision{},
	}
	s.graphs[taskID] = graph
	s.doneChans[taskID] = make(chan struct{})

	s.finalize(graph)

	if graph.Status != schemas.GraphCompleted {
		t.Errorf("expected GraphCompleted, got %s", graph.Status)
	}
}
