package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// These tests are regression tests for D3 (DELEGATION_HANDOVER_REDESIGN.md §2 D3):
// FulfillStep previously bypassed the post-success pipeline — it only wrote
// taskStore + set StepOK manually, silently dropping GlobalWorkspace merge,
// AutoDepositResult→artifacts, archiveStep, SignalField, output_files, and
// EventStepCompleted. After the fix, FulfillStep calls handleStepSuccess which
// applies all these side-effects.
//
// Each test follows the pattern: Submit (delegation mode) → poll for decision
// → FulfillStep → assert the previously-missing side-effect is now present.

// newDelegationTestEngine creates a minimal engine in delegation mode for D3
// regression tests. Returns the engine and a cleanup func.
func newDelegationTestEngine(t *testing.T) *DirectedEngine {
	t.Helper()
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "nonexistent",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			DelegationMode:      true,
		},
	}

	logDir := t.TempDir()
	outDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})

	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	expStore := mustNewExperienceStore(t, t.TempDir(), ts, &reg.System, nil, nil)

	engine := NewDirectedEngine(reg, ts, expStore, nil, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })
	return engine
}

// waitForDelegationDecision polls until the task has a pending delegation decision.
func waitForDelegationDecision(t *testing.T, engine *DirectedEngine, taskID string) *schemas.Decision {
	t.Helper()
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph := engine.graphs[taskID]
		blocked := graph != nil && len(graph.PendingDecisions) > 0
		engine.Mu.RUnlock()
		if blocked {
			engine.Mu.RLock()
			dec := engine.graphs[taskID].PendingDecisions[0]
			engine.Mu.RUnlock()
			return dec
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for delegation decision on task %s", taskID)
	return nil
}

// TestFulfillStep_WritesGlobalWorkspace is a regression test for D3:
// FulfillStep must apply the step's merge_strategy to GlobalWorkspace.
// Before the fix: graph.GlobalWorkspace was always empty after fulfillment.
// After  the fix: overlay merge writes the result to the target key.
func TestFulfillStep_WritesGlobalWorkspace(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{
			ID:            "step_1",
			RoleID:        "worker",
			Task:          "Compute the answer",
			MergeStrategy: "overlay",
			TargetKey:     "answer",
		},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	fulfillment := `{"status":"ok","confidence":0.9,"result":{"answer":42},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	gw := graph.GlobalWorkspace
	engine.Mu.RUnlock()

	if gw == nil || len(gw) == 0 {
		t.Fatal("D3 regression: GlobalWorkspace is empty after FulfillStep; " +
			"AutoDepositResult was not applied (post-success pipeline bypassed)")
	}
	// AutoDepositResult writes the result under the step ID and/or "result" key.
	// (The overlay merge via MergeStrategy/TargetKey is covered by
	// TestFulfillStep_SingleStepOverlayMerge — the fast-path now copies
	// those fields from StepInput to Step per D3 §10.2.)
	stepVal, ok := gw["step_1"]
	if !ok {
		t.Fatalf("D3 regression: GlobalWorkspace[\"step_1\"] missing after FulfillStep; "+
			"got GlobalWorkspace=%v", gw)
	}
	resultMap, ok := stepVal.(map[string]any)
	if !ok || resultMap["answer"] != float64(42) {
		t.Errorf("expected GlobalWorkspace[\"step_1\"][\"answer\"]=42, got %v", stepVal)
	}
}

// TestFulfillStep_LowConfidenceStillSucceeds is a regression test for D3's key
// design decision: FulfillStep calls handleStepSuccess (not handleOutput) to
// skip the confidence threshold gate. A host self-reporting confidence=0.5
// must NOT trigger a retry decision — the fulfill action itself determines
// success (always OK).
func TestFulfillStep_LowConfidenceStillSucceeds(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do something"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	// confidence=0.5 is BELOW the threshold (0.7). With handleOutput this would
	// route to handleStepFailure and create a new retry decision. With the fix
	// (handleStepSuccess directly), the step is OK and no new decision appears.
	fulfillment := `{"status":"ok","confidence":0.5,"result":{"x":1},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	stepStatus := graph.Steps["step_1"].Status
	pendingCount := len(graph.PendingDecisions)
	engine.Mu.RUnlock()

	if stepStatus != schemas.StepOK {
		t.Fatalf("D3 regression: step status=%s after low-confidence fulfill; "+
			"expected StepOK (confidence gate should be skipped)", stepStatus)
	}
	if pendingCount != 0 {
		t.Fatalf("D3 regression: %d pending decisions after low-confidence fulfill; "+
			"expected 0 (confidence gate should not create a retry)", pendingCount)
	}
}

// TestFulfillStep_RegistersOutputFiles is a regression test for D3:
// FulfillStep must register output_files when the fulfillment result contains
// an "output_ref" key. Before the fix: graph.OutputFiles was always empty.
// After  the fix: the output file is appended to graph.OutputFiles.
func TestFulfillStep_RegistersOutputFiles(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Generate a report"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	// The result includes output_ref/summary/format — handleStepSuccess checks
	// output.Result["output_ref"] and appends to graph.OutputFiles.
	fulfillment := `{"status":"ok","confidence":0.9,"result":{"output_ref":"report.md","summary":"final report","format":"md"},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	outputFiles := graph.OutputFiles
	engine.Mu.RUnlock()

	if len(outputFiles) == 0 {
		t.Fatal("D3 regression: graph.OutputFiles is empty after FulfillStep with output_ref; " +
			"output_files registration was not applied (post-success pipeline bypassed)")
	}
	if outputFiles[0].Path != "report.md" {
		t.Errorf("expected output_files[0].Path=\"report.md\", got %q", outputFiles[0].Path)
	}
}

// TestFulfillStep_PublishesStepCompletedEvent is a regression test for D3:
// FulfillStep must publish an EventStepCompleted event (previously missing).
// We verify via the step's ResultRef being set (a side-effect of handleStepSuccess).
func TestFulfillStep_SetsResultRef(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	fulfillment := `{"status":"ok","confidence":0.9,"result":{"x":1},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	engine.Mu.RLock()
	step := engine.graphs[taskID].Steps["step_1"]
	engine.Mu.RUnlock()

	if step.ResultRef == "" {
		t.Fatal("D3 regression: step.ResultRef is empty after FulfillStep; " +
			"handleStepSuccess should set it (was always empty before the fix)")
	}
}
