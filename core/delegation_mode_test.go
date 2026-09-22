package core

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// mkdirTemp creates a temp directory and registers a best-effort cleanup that
// ignores errors. We deliberately avoid t.TempDir() for engine directories
// because the engine's background goroutines (asset sentry, step runners) write
// to them asynchronously; t.TempDir()'s RemoveAll fails the test on Windows if
// the directory is not empty at cleanup time. Best-effort cleanup avoids that
// flaky failure while still reclaiming space in the common case.
func mkdirTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "delegtest-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDelegationMode_SingleStepTask(t *testing.T) {
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "nonexistent",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"worker": {
				ID:             "worker",
				BaseCapability: "text",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			DelegationMode:      true,
		},
	}

	logDir := t.TempDir()
	outDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})

	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	expStore, _ := store.NewExperienceStore(t.TempDir(), ts, &reg.System, nil, nil)

	engine := NewDirectedEngine(reg, ts, expStore, nil, logger, nil, outDir, outDir, nil)
	defer engine.Stop()

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Think for me"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	var graph *schemas.TaskGraph
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		blocked := graph != nil && graph.Status == schemas.GraphBlocked
		engine.Mu.RUnlock()
		if blocked {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	engine.Mu.RLock()
	if graph == nil {
		engine.Mu.RUnlock()
		t.Fatalf("graph not found")
	}
	if graph.Status != schemas.GraphBlocked {
		engine.Mu.RUnlock()
		t.Fatalf("expected graph blocked, got %v", graph.Status)
	}
	if !graph.IsSmartRouted {
		engine.Mu.RUnlock()
		t.Errorf("expected smart-routed graph")
	}
	if len(graph.PendingDecisions) == 0 {
		engine.Mu.RUnlock()
		t.Fatalf("expected pending decision")
	}
	dec := graph.PendingDecisions[0]
	engine.Mu.RUnlock()
	if dec.Type != schemas.DecisionDelegationRequired {
		t.Errorf("expected delegation decision, got %v", dec.Type)
	}

	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatalf("expected prompt in decision context")
	}
	if _, ok := prompt["system_prompt"]; !ok {
		t.Errorf("expected system_prompt in delegation prompt")
	}
	if _, ok := prompt["user_prompt"]; !ok {
		t.Errorf("expected user_prompt in delegation prompt")
	}

	fulfillment := `{
		"status": "ok",
		"confidence": 0.9,
		"result": {"answer": "I have thought for you"},
		"capability": "text"
	}`
	err = engine.FulfillStep(taskID, dec.ID, fulfillment)
	if err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

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
		t.Errorf("expected graph completed, got %v", graph.Status)
	}

	res, _ := ts.Get(context.Background(), taskID, "step_1")
	if res == nil {
		t.Fatalf("expected result in task store")
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", res.Data)
	}
	if data["answer"] != "I have thought for you" {
		t.Errorf("unexpected result: %v", data)
	}
}

func TestDelegationMode_MultiStepTask(t *testing.T) {
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "nonexistent",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"worker": {
				ID:             "worker",
				BaseCapability: "text",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			DelegationMode:      true,
		},
	}

	logDir := t.TempDir()
	outDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})

	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	expStore, _ := store.NewExperienceStore(t.TempDir(), ts, &reg.System, nil, nil)

	engine := NewDirectedEngine(reg, ts, expStore, nil, logger, nil, outDir, outDir, nil)
	defer engine.Stop()

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "First step"},
		{ID: "step_2", RoleID: "worker", Task: "Second step", DependsOn: []string{"step_1"}},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	var graph *schemas.TaskGraph
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		blocked := graph != nil && graph.Status == schemas.GraphBlocked
		engine.Mu.RUnlock()
		if blocked {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	engine.Mu.RLock()
	if graph == nil {
		engine.Mu.RUnlock()
		t.Fatalf("graph not found")
	}
	if graph.Status != schemas.GraphBlocked {
		engine.Mu.RUnlock()
		t.Fatalf("expected graph blocked, got %v", graph.Status)
	}
	if len(graph.PendingDecisions) == 0 {
		engine.Mu.RUnlock()
		t.Fatalf("expected pending decision")
	}
	dec := graph.PendingDecisions[0]
	engine.Mu.RUnlock()
	if dec.Type != schemas.DecisionDelegationRequired {
		t.Errorf("expected delegation decision, got %v", dec.Type)
	}

	fulfillment := `{
		"status": "ok",
		"confidence": 0.9,
		"result": {"answer": "first done"},
		"capability": "text"
	}`
	err = engine.FulfillStep(taskID, dec.ID, fulfillment)
	if err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		hasPending := graph != nil && len(graph.PendingDecisions) > 0
		engine.Mu.RUnlock()
		if hasPending {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	engine.Mu.RLock()
	noPending := graph == nil || len(graph.PendingDecisions) == 0
	var dec2 *schemas.Decision
	if !noPending {
		dec2 = graph.PendingDecisions[0]
	}
	engine.Mu.RUnlock()
	if noPending {
		t.Fatalf("expected second delegation decision after fulfilling first")
	}
	if dec2.Type != schemas.DecisionDelegationRequired {
		t.Errorf("expected second delegation decision, got %v", dec2.Type)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// newDelegationEngine builds a DirectedEngine with DelegationMode enabled and a
// single "worker" role. No real providers are configured, so every step blocks
// on a DecisionDelegationRequired instead of calling an LLM — this is what lets
// the suite exercise the full orchestration path without hitting 429s.
func newDelegationEngine(t *testing.T) (*DirectedEngine, *config.Registry) {
	t.Helper()
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "nonexistent",
		Providers:       make(map[string]*config.ProviderConfig),
		Roles: map[string]*config.Role{
			"worker": {
				ID:             "worker",
				BaseCapability: "text",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			DelegationMode:      true,
		},
	}

	logDir := mkdirTemp(t)
	outDir := mkdirTemp(t)
	logger, _ := NewLogger(logDir, &config.SystemSettings{})

	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	expStore, _ := store.NewExperienceStore(mkdirTemp(t), ts, &reg.System, nil, nil)

	engine := NewDirectedEngine(reg, ts, expStore, nil, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })
	return engine, reg
}

// waitForBlocked polls the graph until it reaches GraphBlocked with at least one
// pending decision, or fails the test after a timeout.
func waitForBlocked(t *testing.T, engine *DirectedEngine, taskID string) *schemas.TaskGraph {
	t.Helper()
	var graph *schemas.TaskGraph
	for i := 0; i < 30; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		blocked := graph != nil && graph.Status == schemas.GraphBlocked && len(graph.PendingDecisions) > 0
		engine.Mu.RUnlock()
		if blocked {
			return graph
		}
		time.Sleep(100 * time.Millisecond)
	}
	engine.Mu.RLock()
	graph = engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph == nil {
		t.Fatalf("graph %q not found", taskID)
	}
	t.Fatalf("expected graph blocked with pending decision, got status=%v decisions=%d",
		graph.Status, len(graph.PendingDecisions))
	return nil
}

// fulfillDelegation fulfils the first pending delegation decision with the given
// result payload and waits for the graph to settle.
func fulfillDelegation(t *testing.T, engine *DirectedEngine, taskID, answer string) {
	t.Helper()
	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph == nil || len(graph.PendingDecisions) == 0 {
		t.Fatalf("no pending decision to fulfill for task %q", taskID)
	}
	dec := graph.PendingDecisions[0]
	fulfillment := `{"status":"ok","confidence":0.9,"result":{"answer":"` + answer + `"},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}
}

// TC-SCH-02 (Safety audit): A high-risk instruction must be intercepted by the
// delegation gate before any execution occurs. In delegation mode the engine
// blocks on a DecisionDelegationRequired rather than running the destructive
// command, proving the safety boundary holds without needing a real provider.
func TestDelegationMode_HighRiskTaskIntercepted(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "rm -rf /"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	graph := waitForBlocked(t, engine, taskID)
	dec := graph.PendingDecisions[0]
	if dec.Type != schemas.DecisionDelegationRequired {
		t.Fatalf("expected delegation decision for high-risk task, got %v", dec.Type)
	}
	// The destructive task must NOT have executed: step must still be pending/blocked.
	step := graph.Steps["step_1"]
	if step == nil {
		t.Fatal("step_1 not found")
	}
	if step.Status == schemas.StepOK {
		t.Errorf("high-risk task executed (StepOK) instead of being intercepted")
	}
	// The prompt context must carry the original task text so a human/outer agent
	// can review what was requested.
	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatal("expected prompt in decision context")
	}
	if _, ok := prompt["user_prompt"]; !ok {
		t.Error("expected user_prompt in delegation context for review")
	}
}

// TC-EXP-01 (Dynamic role synthesis): The delegation decision must carry the
// role_id and target_capability so the fulfilling agent knows what capability
// to satisfy. Verifies the spawner delegation block populates the result map.
func TestDelegationMode_PromptContainsRoleAndCapability(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "analyze the data"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	graph := waitForBlocked(t, engine, taskID)
	dec := graph.PendingDecisions[0]
	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatal("expected prompt in decision context")
	}
	if prompt["role_id"] != "worker" {
		t.Errorf("expected role_id=worker in prompt, got %v", prompt["role_id"])
	}
	if prompt["target_capability"] != "text" {
		t.Errorf("expected target_capability=text, got %v", prompt["target_capability"])
	}
	if _, ok := prompt["system_prompt"]; !ok {
		t.Error("expected system_prompt in delegation prompt")
	}
	if prompt["user_prompt"] != "analyze the data" {
		t.Errorf("expected user_prompt to echo the task, got %v", prompt["user_prompt"])
	}
}

// TC-CTX-01 (Context branch): Submitting a task must seed a context tree with a
// root node whose Intent matches the task and whose Status is NodeActive.
func TestDelegationMode_ContextTreeRootCreated(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "understand the requirement"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	graph := waitForBlocked(t, engine, taskID)
	if len(graph.ContextTree) == 0 {
		t.Fatal("expected non-empty context tree")
	}
	root, ok := graph.ContextTree["root"]
	if !ok {
		t.Fatal("expected 'root' node in context tree")
	}
	if root.Status != schemas.NodeActive {
		t.Errorf("expected root node active, got %v", root.Status)
	}
	if root.Intent != "understand the requirement" {
		t.Errorf("expected root intent to match task, got %q", root.Intent)
	}
	if graph.CurrentNodeID != "root" {
		t.Errorf("expected current node = root, got %q", graph.CurrentNodeID)
	}
}

// TC-GRP-03 (Parallel): Two independent steps (no DependsOn between them) must
// both produce delegation decisions, proving the DAG can fan out without a real
// provider. Fulfilling both completes the graph.
func TestDelegationMode_ParallelIndependentSteps(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_a", RoleID: "worker", Task: "branch A"},
		{ID: "step_b", RoleID: "worker", Task: "branch B"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	graph := waitForBlocked(t, engine, taskID)
	// At least one delegation decision must be pending; for independent steps
	// the engine may surface one or both at once.
	if len(graph.PendingDecisions) == 0 {
		t.Fatal("expected pending delegation decision(s) for parallel steps")
	}
	for _, dec := range graph.PendingDecisions {
		if dec.Type != schemas.DecisionDelegationRequired {
			t.Errorf("expected delegation decision, got %v", dec.Type)
		}
	}

	// Fulfill every pending decision until the graph completes.
	for i := 0; i < 10; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		completed := graph != nil && graph.Status == schemas.GraphCompleted
		pendingCount := 0
		var firstDec *schemas.Decision
		if !completed && graph != nil {
			pendingCount = len(graph.PendingDecisions)
			if pendingCount > 0 {
				firstDec = graph.PendingDecisions[0]
			}
		}
		engine.Mu.RUnlock()
		if completed {
			break
		}
		if pendingCount == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		fulfillment := `{"status":"ok","confidence":0.9,"result":{"answer":"done"},"capability":"text"}`
		if err := engine.FulfillStep(taskID, firstDec.ID, fulfillment); err != nil {
			t.Fatalf("FulfillStep failed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	engine.Mu.RLock()
	graph = engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph.Status != schemas.GraphCompleted {
		t.Errorf("expected graph completed, got %v", graph.Status)
	}
}

// Extends the multi-step case to a full A→B→C dependency chain, verifying that
// each fulfillment unblocks exactly the next downstream step.
func TestDelegationMode_DependencyChain(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "a", RoleID: "worker", Task: "step A"},
		{ID: "b", RoleID: "worker", Task: "step B", DependsOn: []string{"a"}},
		{ID: "c", RoleID: "worker", Task: "step C", DependsOn: []string{"b"}},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	seen := []string{}
	for i := 0; i < 30; i++ {
		engine.Mu.RLock()
		graph := engine.graphs[taskID]
		if graph == nil {
			engine.Mu.RUnlock()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Race fix: hold RLock while reading graph.Status and graph.PendingDecisions
		// so we don't race with addDecision/finalize writes (now under Mu.Lock).
		status := graph.Status
		pendingCount := len(graph.PendingDecisions)
		var firstDec *schemas.Decision
		if pendingCount > 0 {
			firstDec = graph.PendingDecisions[0]
		}
		engine.Mu.RUnlock()

		if status == schemas.GraphCompleted {
			break
		}
		if pendingCount > 0 {
			seen = append(seen, firstDec.StepID)
			fulfillment := `{"status":"ok","confidence":0.9,"result":{"answer":"ok"},"capability":"text"}`
			if err := engine.FulfillStep(taskID, firstDec.ID, fulfillment); err != nil {
				t.Fatalf("FulfillStep failed at %s: %v", firstDec.StepID, err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph.Status != schemas.GraphCompleted {
		t.Errorf("expected graph completed, got %v (seen=%v)", graph.Status, seen)
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 sequential delegations, got %d (%v)", len(seen), seen)
	}
}

// CancelTask must mark the graph as GraphCancelled and return no error for a
// known task.
func TestDelegationMode_CancelTask(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "cancellable work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the graph to exist (it registers synchronously on Submit).
	var graph *schemas.TaskGraph
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		engine.Mu.RUnlock()
		if graph != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if graph == nil {
		t.Fatal("graph not registered after Submit")
	}

	if err := engine.CancelTask(taskID); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	engine.Mu.RLock()
	graph = engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph.Status != schemas.GraphCancelled {
		t.Errorf("expected graph cancelled, got %v", graph.Status)
	}
	// Double-cancel is idempotent in the current implementation: the graph is
	// already terminal, so the observable contract is that the status remains
	// GraphCancelled (not that an error is returned).
	if err := engine.CancelTask(taskID); err != nil {
		t.Logf("second CancelTask returned error (acceptable): %v", err)
	}
	engine.Mu.RLock()
	graph = engine.graphs[taskID]
	engine.Mu.RUnlock()
	if graph.Status != schemas.GraphCancelled {
		t.Errorf("expected graph still cancelled after double-cancel, got %v", graph.Status)
	}
}

// GetStatus must return the live status dict for a delegated task.
func TestDelegationMode_GetStatus(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "status check"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	waitForBlocked(t, engine, taskID)

	status, ok := engine.GetStatus(taskID)
	if !ok {
		t.Fatal("GetStatus returned false for active task")
	}
	gotID, _ := status["task_id"].(string)
	if gotID != taskID {
		t.Errorf("expected task_id=%q, got %q", taskID, gotID)
	}
	// status is stored as a typed GraphStatus, not a plain string, so compare
	// via string conversion rather than a .(string) type assertion.
	gotStatus := fmt.Sprintf("%v", status["status"])
	if gotStatus != string(schemas.GraphBlocked) {
		t.Errorf("expected status=blocked, got %q", gotStatus)
	}
}

// ─── FulfillStep error paths ──────────────────────────────────────────────

func TestDelegationMode_FulfillStep_UnknownTaskID(t *testing.T) {
	engine, _ := newDelegationEngine(t)
	err := engine.FulfillStep("nonexistent_task", "dec_1", `{"status":"ok","confidence":0.9,"result":{"x":1}}`)
	if err == nil {
		t.Fatal("expected error for unknown task ID")
	}
}

func TestDelegationMode_FulfillStep_UnknownDecisionID(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	waitForBlocked(t, engine, taskID)

	err = engine.FulfillStep(taskID, "nonexistent_decision", `{"status":"ok","confidence":0.9,"result":{"x":1}}`)
	if err == nil {
		t.Fatal("expected error for unknown decision ID")
	}
}

func TestDelegationMode_FulfillStep_FailedStatusRejected(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)
	dec := graph.PendingDecisions[0]

	// A fulfillment whose parsed status is "failed" must be rejected.
	failed := `{"status":"failed","confidence":0.5,"result":{"error":"boom"}}`
	if err := engine.FulfillStep(taskID, dec.ID, failed); err == nil {
		t.Fatal("expected error for failed-status fulfillment")
	}
}

func TestDelegationMode_FulfillStep_NonDelegationDecisionRejected(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)

	// Inject a non-delegation decision to exercise the type guard in FulfillStep.
	dec := &schemas.Decision{
		ID:     "dec_low_conf",
		StepID: "step_1",
		Type:   schemas.DecisionLowConfidence,
	}
	engine.Mu.Lock()
	graph.PendingDecisions = append(graph.PendingDecisions, dec)
	engine.Mu.Unlock()

	if err := engine.FulfillStep(taskID, dec.ID, `{"status":"ok","confidence":0.9,"result":{"x":1}}`); err == nil {
		t.Fatal("expected error fulfilling a non-delegation decision")
	}
}

func TestDelegationMode_FulfillStep_StepNotFound(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)

	// Inject a delegation decision whose StepID points to a nonexistent step.
	dec := &schemas.Decision{
		ID:     "dec_orphan",
		StepID: "ghost_step",
		Type:   schemas.DecisionDelegationRequired,
	}
	engine.Mu.Lock()
	graph.PendingDecisions = append(graph.PendingDecisions, dec)
	engine.Mu.Unlock()

	if err := engine.FulfillStep(taskID, dec.ID, `{"status":"ok","confidence":0.9,"result":{"x":1}}`); err == nil {
		t.Fatal("expected error for decision pointing to nonexistent step")
	}
}
