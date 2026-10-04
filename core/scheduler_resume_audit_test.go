package core

import (
	"context"
	"sync"
	"testing"

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
	logger := mustNewLogger(t, mustTempDir(t, "vortex-test"), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	taskDir := mustTempDir(t, "vortex-test")
	taskBackend := store.NewFileTaskBackend(taskDir)
	taskStore := store.NewTaskStore(taskBackend)

	s := &DirectedEngine{
		logger:      logger,
		outputBase:  mustTempDir(t, "vortex-test"),
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
		Status:      schemas.GraphBlocked,
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

	// Verify the restart code ran: a cancelFunc should be installed for
	// this task. Before the fix (L-N5), cancelFuncs[taskID] would be nil
	// because the restart block was missing.
	cancel, ok := s.cancelFuncs[taskID]
	if !ok || cancel == nil {
		t.Fatal("cancelFuncs[taskID] not set — run() goroutine not restarted (L-N5 bug)")
	}

	// Verify graph transitioned out of GraphBlocked.
	s.Mu.RLock()
	graphStatus := s.graphs[taskID].Status
	s.Mu.RUnlock()
	if graphStatus == schemas.GraphBlocked {
		t.Fatal("graph still GraphBlocked after FulfillStep — resume didn't happen")
	}

	// Cleanup: cancel to let the goroutine exit gracefully.
	cancel()
	s.bgWg.Wait()
}

// TestFulfillDelegation_RestartsRunGoroutine is a regression test for audit
// L-N6: FulfillDelegation had the same silent-stall bug as L-N5. We verify
// the cancelFunc is installed after fulfillment, proving the restart ran.
//
// Note: FulfillDelegation calls handleOutput which requires an AssetManager.
// We initialize a minimal one to avoid nil-panics.
func TestFulfillDelegation_RestartsRunGoroutine(t *testing.T) {
	logger := mustNewLogger(t, mustTempDir(t, "vortex-test"), nil)
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	taskDir := mustTempDir(t, "vortex-test")
	taskBackend := store.NewFileTaskBackend(taskDir)
	taskStore := store.NewTaskStore(taskBackend)

	reg := &config.Registry{}
	s := &DirectedEngine{
		logger:      logger,
		outputBase:  mustTempDir(t, "vortex-test"),
		graphs:      make(map[string]*schemas.TaskGraph),
		doneChans:   make(map[string]chan struct{}),
		cancelFuncs: make(map[string]context.CancelFunc),
		bgWg:        sync.WaitGroup{},
		notifyChan:  make(chan struct{}, 1),
		taskStore:   taskStore,
		registry:    reg,
		assets:      NewAssetManager(mustTempDir(t, "vortex-test"), 0, reg),
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	taskID := "test_ln6_task"
	stepID := "step1"

	s.graphs[taskID] = &schemas.TaskGraph{
		TaskID:      taskID,
		Status:      schemas.GraphBlocked,
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

	// Verify the restart code ran.
	cancel, ok := s.cancelFuncs[taskID]
	if !ok || cancel == nil {
		t.Fatal("cancelFuncs[taskID] not set — run() goroutine not restarted (L-N6 bug)")
	}

	// Cleanup.
	cancel()
	s.bgWg.Wait()
}

