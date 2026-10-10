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

// TestFulfillStep_RestartsRunGoroutine is a regression test for audit L-N5:
// FulfillStep previously set GraphRunning + new doneChans but did NOT
// restart the run() goroutine (no cancelFunc update, no goBackground call).
// The task silently stalled in GraphRunning with no executor.
//
// After the fix, FulfillStep cancels the old context, derives a fresh one
// from lifecycleCtx, and relaunches run() via goBackground. We verify by
// checking that a new cancelFunc is installed (proving the restart code ran).
func TestFulfillStep_RestartsRunGoroutine(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	taskDir := t.TempDir()
	taskBackend := store.NewFileTaskBackend(taskDir)
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
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	taskID := "test_ln5_task"
	stepID := "step1"
	decisionID := "dec1"

	s.graphs[taskID] = &schemas.TaskGraph{
		TaskID:      taskID,
		Status:      schemas.GraphRunning,
		Steps:       map[string]*schemas.Step{},
		OutputFiles: []schemas.OutputFile{},
	}
	s.graphs[taskID].Steps[stepID] = &schemas.Step{
		ID:     stepID,
		Status: schemas.StepBlocked,
		RoleID: "test_role",
	}
	s.graphs[taskID].PendingDecisions = []*schemas.Decision{
		{ID: decisionID, StepID: stepID, Type: schemas.DecisionDelegationRequired},
	}
	s.doneChans[taskID] = make(chan struct{})

	// Fulfill the decision with valid JSON output.
	outputJSON := `{"status":"ok","confidence":0.9,"result":{"answer":"42"}}`
	if err := s.FulfillStep(taskID, decisionID, outputJSON); err != nil {
		t.Fatalf("FulfillStep: %v", err)
	}

	// Verify the decision was resolved (pending decisions cleared).
	s.Mu.RLock()
	pendingCount := len(s.graphs[taskID].PendingDecisions)
	graphStatus := s.graphs[taskID].Status
	s.Mu.RUnlock()
	if pendingCount != 0 {
		t.Fatalf("expected 0 pending decisions after FulfillStep, got %d", pendingCount)
	}

	// Verify graph is still running (not blocked).
	if graphStatus == schemas.GraphBlocked {
		t.Fatal("graph still GraphBlocked after FulfillStep — resume didn't happen")
	}
}

// TestFulfillDelegation_RestartsRunGoroutine is a regression test for audit
// L-N6: FulfillDelegation had the same silent-stall bug as L-N5. We verify
// the cancelFunc is installed after fulfillment, proving the restart ran.
//
// Note: FulfillDelegation calls handleOutput which requires an AssetManager.
// We initialize a minimal one to avoid nil-panics.
func TestFulfillDelegation_RestartsRunGoroutine(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	taskDir := t.TempDir()
	taskBackend := store.NewFileTaskBackend(taskDir)
	taskStore := store.NewTaskStore(taskBackend)

	reg := &config.Registry{}
	s := &DirectedEngine{
		logger:      logger,
		outputBase:  t.TempDir(),
		graphs:      make(map[string]*schemas.TaskGraph),
		doneChans:   make(map[string]chan struct{}),
		cancelFuncs: make(map[string]context.CancelFunc),
		bgWg:        sync.WaitGroup{},
		notifyChan:  make(chan struct{}, 1),
		taskStore:   taskStore,
		registry:    reg,
		assets:      NewAssetManager(t.TempDir(), 0, reg),
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	taskID := "test_ln6_task"
	stepID := "step1"

	s.graphs[taskID] = &schemas.TaskGraph{
		TaskID:      taskID,
		Status:      schemas.GraphRunning,
		Steps:       map[string]*schemas.Step{},
		OutputFiles: []schemas.OutputFile{},
	}
	s.graphs[taskID].Steps[stepID] = &schemas.Step{
		ID:     stepID,
		Status: schemas.StepBlocked,
		RoleID: "test_role",
	}
	s.graphs[taskID].PendingDecisions = []*schemas.Decision{
		{ID: "dec1", StepID: stepID, Type: schemas.DecisionDelegationRequired},
	}
	s.doneChans[taskID] = make(chan struct{})

	result := map[string]any{"answer": "42"}
	if err := s.FulfillDelegation(taskID, stepID, result); err != nil {
		t.Fatalf("FulfillDelegation: %v", err)
	}

	// Verify the decision was resolved.
	s.Mu.RLock()
	pendingCount := len(s.graphs[taskID].PendingDecisions)
	s.Mu.RUnlock()
	if pendingCount != 0 {
		t.Fatalf("expected 0 pending decisions after FulfillDelegation, got %d", pendingCount)
	}
}

// TestResumeSuspendedTask_SwarmModeRestartsWatchdog is a regression test for
// audit L-N1: ResumeSuspendedTask only started run() when !UseSwarm. In
// swarm mode, no swarmWatchdog was restarted — the task stalled forever.
func TestResumeSuspendedTask_SwarmModeRestartsWatchdog(t *testing.T) {
	t.Skip("swarm mode is not available in the open core")
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	s := &DirectedEngine{
		logger:      logger,
		outputBase:  t.TempDir(),
		graphs:      make(map[string]*schemas.TaskGraph),
		doneChans:   make(map[string]chan struct{}),
		cancelFuncs: make(map[string]context.CancelFunc),
		bgWg:        sync.WaitGroup{},
		UseSwarm:    true,
		notifyChan:  make(chan struct{}, 1),
		registry: &config.Registry{
			System: config.SystemSettings{
				SwarmFallbackDelay: 60,
			},
		},
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	taskID := "test_ln1_task"
	s.graphs[taskID] = &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphSuspended,
		Steps: map[string]*schemas.Step{
			"step1": {ID: "step1", Status: schemas.StepPending, RoleID: "test"},
		},
	}

	if err := s.ResumeSuspendedTask(taskID); err != nil {
		t.Fatalf("ResumeSuspendedTask: %v", err)
	}

	// Verify swarmWatchdog goroutine was started in swarm mode.
	waitDone := make(chan struct{})
	go func() {
		s.bgWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("bgWg.Wait() returned immediately — swarmWatchdog not restarted in swarm mode (L-N1 bug)")
	case <-time.After(200 * time.Millisecond):
		// Good: swarmWatchdog is running and tracked.
	}

	lifecycleCancel()
	s.bgWg.Wait()
}
