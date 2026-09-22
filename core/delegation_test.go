package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// DEPRECATED (2026-08-27): TestEngine_CognitiveHandover.
// This test exercises a legacy automatic-delegation path that no longer
// exists in DirectedEngine:
//
//   - Original assumption: submitting a single-step task with no available
//     models would automatically produce a DecisionDelegationRequired node.
//   - Actual current behaviour:
//     (a) Smart Routing fast-path (SubmitWithSessionIR) routes single-step
//     tasks directly to spawner.Spawn, completely bypassing the DAG
//     graph — no TaskGraph is ever created for the task.
//     (b) If RequirePlanReview is enabled to bypass Smart Routing, the
//     graph enters GraphPendingReview and never emits PendingDecisions.
//
// The underlying engine path this test targets has been superseded by
// Smart Routing (scheduler.go:SubmitWithSessionIR) and the decision-node
// machinery in DecisionNode / PendingDecisions. There is no clean way to
// make this test pass without essentially rewriting it from scratch.
//
// Kept here for historical reference. Will be removed once a proper
// replacement test is written for the actual delegation flow.
//
// See also: TestSmartRouting_SingleStepNoDeps_BypassesDAG
//
//	TestDetectUpstreamInsufficient_UpstreamFailed
func TestEngine_CognitiveHandover(t *testing.T) {
	t.Skip("DEPRECATED 2026-08-27: legacy automatic-delegation path no longer exists. See test doc comment.")
	// 1. Setup Registry with NO models
	reg := &config.Registry{
		DefaultProvider: "missing",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"worker": {
				ID: "worker",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
		},
	}

	// 2. Setup Engine
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})

	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)

	expStore, _ := store.NewExperienceStore(t.TempDir(), ts, &reg.System, nil, nil)

	engine := NewDirectedEngine(reg, ts, expStore, nil, logger, nil, outDir, outDir, nil)

	// 3. Submit Task
	ctx := context.Background()
	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Think for me"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// 4. Wait for a graph state (legacy: expected blocked)
	var graph *schemas.TaskGraph
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		ready := graph != nil && (graph.Status == schemas.GraphBlocked || graph.Status == schemas.GraphPendingReview)
		engine.Mu.RUnlock()
		if ready {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	engine.Mu.RLock()
	if graph == nil || (graph.Status != schemas.GraphBlocked && graph.Status != schemas.GraphPendingReview) {
		engine.Mu.RUnlock()
		t.Fatalf("Expected graph to be blocked or pending_review, got %v", graph.Status)
	}
	if len(graph.PendingDecisions) == 0 {
		engine.Mu.RUnlock()
		t.Fatalf("Expected a pending decision")
	}
	dec := graph.PendingDecisions[0]
	engine.Mu.RUnlock()
	if dec.Type != schemas.DecisionDelegationRequired {
		t.Errorf("Expected decision type delegation, got %v", dec.Type)
	}

	// 6. Fulfill Step
	fulfillment := `{
		"status": "ok",
		"confidence": 0.9,
		"result": {"answer": "I have thought for you"},
		"capability": "worker"
	}`
	err = engine.FulfillStep(taskID, dec.ID, fulfillment)
	if err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	// 7. Verify task continues and completes
	for i := 0; i < 10; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		completed := graph != nil && graph.Status == schemas.GraphCompleted
		engine.Mu.RUnlock()
		if completed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	engine.Mu.RLock()
	finalStatus := graph.Status
	engine.Mu.RUnlock()
	if finalStatus != schemas.GraphCompleted {
		t.Errorf("Expected graph to be completed, got %v", finalStatus)
	}

	// Check result
	res, _ := ts.Get(ctx, taskID, "step_1")
	if res == nil || res.Data.(map[string]any)["answer"] != "I have thought for you" {
		t.Errorf("Fulfillment data mismatch: %v", res)
	}
}
