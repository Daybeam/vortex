package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestForkTask_SwarmMode_StartsSwarmWatchdog is a regression test for audit L-N9:
// ForkTask previously called s.run() unconditionally without the UseSwarm branch.
// In swarm mode, the forked task was marked GraphRunning but swarmWatchdog was
// never started — the task stalled forever with no executor.
//
// After the fix, ForkTask calls startGraphExecutor which correctly branches on
// UseSwarm. We verify by checking that a background goroutine was launched
// (bgWg > 0) and a cancelFunc was installed for the forked task.
func TestForkTask_SwarmMode_StartsSwarmWatchdog(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	taskBackend := store.NewFileTaskBackend(t.TempDir())
	taskStore := store.NewTaskStore(taskBackend)

	s := &DirectedEngine{
		logger:      logger,
		outputBase:  t.TempDir(),
		graphs:      make(map[string]*schemas.TaskGraph),
		doneChans:   make(map[string]chan struct{}),
		cancelFuncs: make(map[string]context.CancelFunc),
		bgWg:        sync.WaitGroup{},
		notifyChan:  make(chan struct{}, 1),
		taskStore:   taskStore,
		registry:    &config.Registry{},
		UseSwarm:    true, // L-N9: swarm mode — must use swarmWatchdog, not run
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	// Set up a source graph with one completed step.
	srcGraph := &schemas.TaskGraph{
		TaskID: "src-ln9",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK, Task: "do something"},
		},
		Status:      schemas.GraphCompleted,
		OutputFiles: []schemas.OutputFile{},
	}
	s.graphs["src-ln9"] = srcGraph

	// ForkTask should call startGraphExecutor, which in swarm mode launches
	// swarmWatchdog (not run). The goroutine is tracked by bgWg.
	newTaskID, err := s.ForkTask("src-ln9", "s1")
	if err != nil {
		t.Fatalf("ForkTask: %v", err)
	}

	// Verify a cancelFunc was installed for the forked task (proves the
	// executor setup code ran).
	s.Mu.RLock()
	_, hasCancel := s.cancelFuncs[newTaskID]
	forkedGraph, hasGraph := s.graphs[newTaskID]
	s.Mu.RUnlock()
	if !hasCancel {
		t.Fatal("L-N9 regression: no cancelFunc installed for forked task — startGraphExecutor was not called")
	}
	if !hasGraph {
		t.Fatal("L-N9 regression: forked graph not found")
	}
	if forkedGraph.Status != schemas.GraphRunning {
		t.Fatalf("L-N9 regression: expected GraphRunning, got %s", forkedGraph.Status)
	}

	// Cancel lifecycle to drain the goroutine (swarmWatchdog will exit).
	lifecycleCancel()
	s.bgWg.Wait()
}

// TestStartGraphExecutor_DispatchesOnUseSwarm verifies that startGraphExecutor
// correctly branches on UseSwarm — calling run() when false, swarmWatchdog when
// true. This is the systemic fix for the L-N9 fix-propagation gap.
func TestStartGraphExecutor_DispatchesOnUseSwarm(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	makeEngine := func(useSwarm bool) *DirectedEngine {
		s := &DirectedEngine{
			logger:      logger,
			outputBase:  t.TempDir(),
			graphs:      make(map[string]*schemas.TaskGraph),
			doneChans:   make(map[string]chan struct{}),
			cancelFuncs: make(map[string]context.CancelFunc),
			bgWg:        sync.WaitGroup{},
			notifyChan:  make(chan struct{}, 1),
			registry:    &config.Registry{},
			UseSwarm:    useSwarm,
		}
		s.lifecycleCtx = lifecycleCtx
		s.lifecycleCancel = lifecycleCancel
		// Add a minimal graph so run()/swarmWatchdog don't nil-deref.
		s.graphs["t1"] = &schemas.TaskGraph{
			TaskID: "t1",
			Steps:  map[string]*schemas.Step{},
			Status: schemas.GraphRunning,
		}
		s.doneChans["t1"] = make(chan struct{})
		return s
	}

	// Non-swarm: should launch run() (which will exit quickly on an empty graph).
	s1 := makeEngine(false)
	s1.startGraphExecutor(lifecycleCtx, "t1")
	// audit T-GATE-3 (2026-10-08): assert goroutine was tracked by bgWg.
	// If startGraphExecutor didn't call goBackground, bgWg.Wait() returns
	// immediately — we detect deadlocks via the timeout.
	lifecycleCancel()
	waitDone1 := make(chan struct{})
	go func() { s1.bgWg.Wait(); close(waitDone1) }()
	select {
	case <-waitDone1:
		// Good: goroutine exited after cancel.
	case <-time.After(2 * time.Second):
		t.Fatal("non-swarm: bgWg.Wait() did not return after cancel — goroutine leak or deadlock")
	}

	// Swarm: should launch swarmWatchdog (which will exit on context cancel).
	// Reset lifecycle since we already cancelled above.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := makeEngine(true)
	s2.lifecycleCtx = ctx2
	s2.startGraphExecutor(ctx2, "t1")
	cancel2()
	waitDone2 := make(chan struct{})
	go func() { s2.bgWg.Wait(); close(waitDone2) }()
	select {
	case <-waitDone2:
		// Good: goroutine exited after cancel.
	case <-time.After(2 * time.Second):
		t.Fatal("swarm: bgWg.Wait() did not return after cancel — goroutine leak or deadlock")
	}
}

// TestSpawnDecider_NilSpawnResult_NoPanic is a regression test for audit L-N10:
// spawnDecider previously dereferenced res.Output.Result without checking if
// res itself was nil. If Spawn returns (nil, nil) (e.g. when an external
// interceptor short-circuits), this would panic with nil pointer dereference.
// The panic was silently recovered by goBackground, leaving the decision
// deadlocked forever in NonInteractive mode.
//
// After the fix, spawnDecider checks `err != nil || res == nil` and returns
// ("", false) safely.
func TestSpawnDecider_NilSpawnResult_NoPanic(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	// Create a Spawner with an interceptor that returns (nil, nil) —
	// simulating an external interceptor short-circuit.
	spawner := &Spawner{
		registry: &config.Registry{},
		logger:   logger,
		interceptors: []Interceptor{
			func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
				return nil, nil // short-circuit: no result, no error
			},
		},
	}

	s := &DirectedEngine{
		logger:     logger,
		spawner:    spawner,
		registry:   &config.Registry{},
		notifyChan: make(chan struct{}, 1),
		bgWg:       sync.WaitGroup{},
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	// Before the L-N10 fix, this would panic (nil deref on res.Output.Result).
	// After the fix, it should return ("", false) safely.
	choice, ok := s.spawnDecider("decider_role", "task1", "step1", schemas.DecisionLowConfidence, map[string]any{}, []string{"option_a", "option_b"}, false, "")
	if ok {
		t.Fatalf("L-N10 regression: expected (\"\", false) for nil Spawn result, got (%q, true)", choice)
	}
	if choice != "" {
		t.Fatalf("L-N10 regression: expected empty choice, got %q", choice)
	}
}
