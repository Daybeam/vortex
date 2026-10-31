package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestSmartRouted_DecisionResolvedFinalizesExactlyOnce pins the invariant that a
// smart-routed (single-step, fast-path) graph publishes exactly ONE terminal
// event when its pending decision is resolved.
//
// Why this test exists: scheduler_submit.go's fast path builds its graph with
// IsSmartRouted:true (scheduler_submit.go:347). Such graphs have no run() loop,
// so SubmitDecision finalizes them INLINE through
// finalizeSmartRoutedAfterDecision (scheduler_status.go:846). That helper closes
// the done channel but deliberately does NOT broadcast — it assumes nothing is
// listening.
//
// If anything also starts run() for such a graph, finalize() runs a second time
// five seconds later: run() is parked on the notify channel with a 5 s safety
// timer (scheduler_dag.go:106-121), wakes on the timer, sees a terminal graph
// with no ready steps and no pending decisions, and calls finalize()
// (scheduler_dag.go:125). finalize() has no idempotency guard, so the second
// call re-publishes task_completed, rewrites manifest.json, and re-runs
// reflect() and deliverArtifacts() — duplicate experience records and duplicate
// webhook deliveries.
//
// The failure mode is invisible in a short-lived container: the first terminal
// event arrives at ~3 s and the harness exits before the 5 s timer fires. Only a
// test that waits out the timer can see it.
func TestSmartRouted_DecisionResolvedFinalizesExactlyOnce(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	t.Cleanup(lifecycleCancel)

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

	taskID := "smart_routed_double_finalize"
	graph := &schemas.TaskGraph{
		TaskID:        taskID,
		Status:        schemas.GraphRunning,
		IsSmartRouted: true,
		Steps: map[string]*schemas.Step{
			// StepBlocked + a pending decision is exactly what the fast path
			// leaves behind after raising a delegation decision.
			"s1": {ID: "s1", RoleID: "appworld_agent", Task: "do the thing", Status: schemas.StepBlocked},
		},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec_smart",
			StepID:  "s1",
			Type:    schemas.DecisionDelegationRequired,
			Options: []string{"fulfill", "skip", "abort"},
		}},
	}
	s.graphs[taskID] = graph
	s.doneChans[taskID] = make(chan struct{})

	var mu sync.Mutex
	terminals := 0
	unsub := DefaultBus.SubscribeWithCancel(string(EventTaskCompleted), func(ev AgentEvent) error {
		if ev.TaskID != taskID {
			return nil
		}
		mu.Lock()
		terminals++
		mu.Unlock()
		return nil
	})
	t.Cleanup(unsub)

	// This is what the fast path does. With the executor call in place it also
	// takes the path this test is about.
	s.startGraphExecutor(lifecycleCtx, taskID)

	// Resolve the decision the way the non-interactive gate does.
	if err := s.SubmitDecision(taskID, "dec_smart", "skip"); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}

	// run()'s safety timer is 5 s; wait past it so a lingering executor has
	// every chance to finalize a second time.
	time.Sleep(6500 * time.Millisecond)

	mu.Lock()
	got := terminals
	mu.Unlock()

	if got != 1 {
		t.Fatalf("terminal events = %d, want exactly 1 (double finalize: "+
			"finalize() re-publishes task_completed, rewrites the manifest and "+
			"re-runs reflect/deliverArtifacts)", got)
	}

	if graph.Status != schemas.GraphCompletedWithSkips {
		t.Errorf("graph status = %s, want %s", graph.Status, schemas.GraphCompletedWithSkips)
	}
}

// TestFinalize_IsIdempotent is the narrow version of the test above: whatever
// the caller does, a second finalize() on an already-terminal graph must be a
// no-op. It pins the guard directly instead of going through the scheduler, so a
// future refactor that moves the finalize producers around cannot silently
// reintroduce the double publish.
func TestFinalize_IsIdempotent(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	t.Cleanup(lifecycleCancel)

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

	taskID := "finalize_idempotent"
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

	var mu sync.Mutex
	terminals := 0
	unsub := DefaultBus.SubscribeWithCancel(string(EventTaskCompleted), func(ev AgentEvent) error {
		if ev.TaskID != taskID {
			return nil
		}
		mu.Lock()
		terminals++
		mu.Unlock()
		return nil
	})
	t.Cleanup(unsub)

	s.finalize(graph)
	s.finalize(graph)

	// reflect/deliverArtifacts/dispatchNotifications run in background goroutines;
	// give them a moment so a second finalize would have published by now.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	got := terminals
	mu.Unlock()

	if got != 1 {
		t.Fatalf("finalize called twice -> %d terminal events, want 1", got)
	}
	if graph.Status != schemas.GraphCompleted {
		t.Errorf("graph status = %s, want %s", graph.Status, schemas.GraphCompleted)
	}
}
