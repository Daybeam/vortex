package core

// e2e_dag_lifecycle_test.go — the unified end-to-end DAG lifecycle test the
// borrow analysis asked for (HARNESS_GUARDRAIL_BORROW_ANALYSIS §5, "Dry Run"):
// a virtual requirement driven through the REAL engine's public surface —
// submit_task → get_task_status (intermediate-state assertions) → wait_task —
// converging the previously fragmented E2E tests (Sieve interception, snapshot
// rollback, chat repetition) under one lifecycle narrative:
//
//	Phase A: submit → [s1 running / s2 pending] → core tool call → dep
//	         transition → completed (+ summary-view progress)
//	Phase B: step failure → step_failed decision → retry → tool call → recovery
//
// Reuses newSpinLoopEngine/registerSpinProvider from
// eval_spin_breaker_e2e_test.go (same package): real DirectedEngine, mock
// provider injected through the global router exactly as production resolves
// it. The provider scripts FULL ProviderResponse values including ToolCalls —
// a write_file via the _core virtual MCP server — because spawner_dospawn.go
// downgrades tool-less status=ok output to partial@0.4 (zero-evidence guard),
// so a realistic lifecycle MUST include real tool execution.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// lifecycleProvider scripts full ProviderResponses per call; call N returns
// script[N mod len]. If gates[N] exists the call BLOCKS until the test closes
// it — freezing the DAG mid-flight so intermediate states are assertable
// through GetStatus.
type lifecycleProvider struct {
	mu     sync.Mutex
	calls  int
	script []*schemas.ProviderResponse
	gates  map[int]chan struct{}
}

func (p *lifecycleProvider) respond() *schemas.ProviderResponse {
	p.mu.Lock()
	p.calls++
	idx := p.calls - 1
	p.mu.Unlock()
	if g, ok := p.gates[idx]; ok {
		<-g
	}
	return p.script[idx%len(p.script)]
}

func (p *lifecycleProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *lifecycleProvider) Name() string { return "mock-lifecycle" }

func (p *lifecycleProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *lifecycleProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.respond(), nil
}

func (p *lifecycleProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	return p.respond(), nil
}

func (p *lifecycleProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }

// newLifecycleEngine mirrors newSpinLoopEngine but uses mkdirTemp (best-effort
// cleanup, delegation_mode_test.go pattern) for the engine's directories: the
// full engine path keeps background goroutines alive past test return, and
// t.TempDir()'s strict RemoveAll fails the test on Windows ("directory is not
// empty") even when every assertion passed.
func newLifecycleEngine(t *testing.T) *DirectedEngine {
	t.Helper()
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "life",
		Providers:       map[string]*config.ProviderConfig{},
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		System: config.SystemSettings{ConfidenceThreshold: 0.7},
	}

	logDir := mkdirTemp(t)
	outDir := mkdirTemp(t)
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	expDir := mkdirTemp(t)
	expStore := mustNewExperienceStore(t, expDir, ts, &reg.System, nil, nil)
	jitDir := mkdirTemp(t)
	jit := NewJITManager(reg, jitDir)

	engine := NewDirectedEngine(reg, ts, expStore, jit, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })
	return engine
}

// lifecycleToolCall / lifecycleFinal build the two scripted response shapes.
func lifecycleToolCall(path string) *schemas.ProviderResponse {
	return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{{
		Name: "write_file", CallID: "call_" + path,
		Arguments: map[string]any{"path": path, "content": "artifact " + path},
	}}}
}

func lifecycleFinal(status string) *schemas.ProviderResponse {
	return &schemas.ProviderResponse{Text: fmt.Sprintf(
		`{"status":%q,"confidence":0.9,"result":{"answer":"%s done"}}`, status, status)}
}

// wireLifecycleProvider registers p as the engine's default + worker provider.
func wireLifecycleProvider(t *testing.T, engine *DirectedEngine, p *lifecycleProvider) {
	t.Helper()
	cfg := &config.ProviderConfig{Provider: "mock-lifecycle", BaseURL: "mock://lifecycle", Model: "life-1"}
	registerSpinProvider(t, cfg, p)
	engine.Mu.Lock()
	engine.registry.Providers["life"] = cfg
	engine.registry.DefaultProvider = "life"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "life"
	}
	engine.Mu.Unlock()
}

// stepStatus reads one step's status from GetStatus(taskID, "full"). Statuses
// are named string types (schemas.StepStatus) — plain .(string) assertions
// fail, so compare via fmt.Sprint (same gotcha as the Telegram handler).
func stepStatus(t *testing.T, engine *DirectedEngine, taskID, stepID string) string {
	t.Helper()
	st, ok := engine.GetStatus(taskID, "full")
	if !ok {
		t.Fatalf("GetStatus(%q) not found", taskID)
	}
	steps, _ := st["steps"].(map[string]any)
	s, _ := steps[stepID].(map[string]any)
	return fmt.Sprint(s["status"])
}

// waitUntil polls cond until true or timeout; fatals with msg.
func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting: %s", msg)
}

// pendingDecisionOfType finds the first pending decision whose type matches
// (DecisionType is a named string — fmt.Sprint comparison).
func pendingDecisionOfType(t *testing.T, engine *DirectedEngine, taskID, dtype string) map[string]any {
	t.Helper()
	st, ok := engine.GetStatus(taskID, "full")
	if !ok {
		return nil
	}
	decs, _ := st["pending_decisions"].([]map[string]any)
	for _, d := range decs {
		if fmt.Sprint(d["type"]) == dtype {
			return d
		}
	}
	return nil
}

// TestE2E_DAGLifecycle_SubmitDepTransitionComplete drives the happy path:
// submit a two-step dependent DAG; freeze it at s1's tool call (s1 running,
// s2 pending) and at s2's tool call (s1 ok, s2 running) to assert TRUE
// intermediate states; then release and assert terminal completion via
// WaitTask, the summary view's progress, and exact provider call counts.
func TestE2E_DAGLifecycle_SubmitDepTransitionComplete(t *testing.T) {
	engine := newLifecycleEngine(t)
	gate1, gate2 := make(chan struct{}), make(chan struct{})
	p := &lifecycleProvider{
		script: []*schemas.ProviderResponse{
			lifecycleToolCall("alpha.txt"), // s1 turn 0: write artifact (gated)
			lifecycleFinal("ok"),           // s1 turn 1: final
			lifecycleToolCall("beta.txt"),  // s2 turn 0: write artifact (gated)
			lifecycleFinal("ok"),           // s2 turn 1: final
		},
		gates: map[int]chan struct{}{0: gate1, 2: gate2},
	}
	wireLifecycleProvider(t, engine, p)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "s1", RoleID: "worker", Task: "produce alpha"},
		{ID: "s2", RoleID: "worker", Task: "consume alpha", DependsOn: []string{"s1"}},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Intermediate #1: s1 mid-tool-call (gated) → s1 running, s2 not launched.
	waitUntil(t, 20*time.Second, "provider never called for s1", func() bool { return p.callCount() >= 1 })
	if got := stepStatus(t, engine, taskID, "s1"); got != "running" {
		t.Errorf("intermediate #1: s1 = %q, want running (gated mid-tool-call)", got)
	}
	if got := stepStatus(t, engine, taskID, "s2"); got != "pending" {
		t.Errorf("intermediate #1: s2 = %q, want pending (dep s1 unfinished)", got)
	}
	close(gate1)

	// Intermediate #2: s1 completed, s2 launched and gated at its tool call.
	waitUntil(t, 20*time.Second, "s2 never launched after s1 completed", func() bool { return p.callCount() >= 3 })
	if got := stepStatus(t, engine, taskID, "s1"); got != "ok" {
		t.Errorf("intermediate #2: s1 = %q, want ok", got)
	}
	if got := stepStatus(t, engine, taskID, "s2"); got != "running" {
		t.Errorf("intermediate #2: s2 = %q, want running", got)
	}
	close(gate2)

	// Terminal: both steps ok, graph completed — via the public WaitTask.
	st, ok := engine.WaitTask(ctx, taskID, 30*time.Second, "full")
	if !ok {
		t.Fatalf("WaitTask returned not-found")
	}
	if gs := fmt.Sprint(st["status"]); gs != "completed" {
		t.Errorf("graph status = %q, want completed; full=%v", gs, st)
	}
	if got := stepStatus(t, engine, taskID, "s1"); got != "ok" {
		t.Errorf("terminal: s1 = %q, want ok", got)
	}
	if got := stepStatus(t, engine, taskID, "s2"); got != "ok" {
		t.Errorf("terminal: s2 = %q, want ok", got)
	}
	if calls := p.callCount(); calls != 4 {
		t.Errorf("provider calls = %d, want exactly 4 (tool+final per step)", calls)
	}

	// Summary view contract (what get_task_status default callers see).
	sum, _ := engine.GetStatus(taskID)
	if prog := fmt.Sprint(sum["progress"]); prog != "2/2" {
		t.Errorf("summary progress = %q, want 2/2", prog)
	}
}

// TestE2E_DAGLifecycle_FailureDecisionRetryRecovery drives the failure leg on
// a two-step DAG (the walker path — single-step submissions take
// SubmitWithSessionIR's inline fast path, which marks any non-error spawn
// result StepOK and never routes StatusFailed to decisions): s1 returns
// status=failed after real tool work → the walker routes it through
// handleStepFailure and raises a step_failed decision offering retry →
// SubmitDecision("retry") re-queues s1 (Broadcast wakes the walker) → the
// retry run does tool work then succeeds → s2 (dep on s1) runs → completion
// with no pending decisions left.
func TestE2E_DAGLifecycle_FailureDecisionRetryRecovery(t *testing.T) {
	engine := newLifecycleEngine(t)
	// The failed FINAL must come AFTER a tool call: a turn-0 text-only
	// "failed" is force-tool-nudged (spawner_dospawn.go:817) and never
	// surfaces as a failure. With a prior tool call the failed status is
	// accepted as the Spawn result and routed to handleStepFailure.
	p := &lifecycleProvider{
		script: []*schemas.ProviderResponse{
			lifecycleToolCall("attempt.txt"), // s1 turn 0: tool work
			lifecycleFinal("failed"),         // s1 turn 1: fails → decision
			lifecycleToolCall("fixed.txt"),   // s1 retry turn 0: tool work
			lifecycleFinal("ok"),             // s1 retry turn 1: final
			lifecycleToolCall("final.txt"),   // s2 turn 0: tool work
			lifecycleFinal("ok"),             // s2 turn 1: final
		},
	}
	wireLifecycleProvider(t, engine, p)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "s1", RoleID: "worker", Task: "do work that fails first"},
		{ID: "s2", RoleID: "worker", Task: "consume the fixed work", DependsOn: []string{"s1"}},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Failure raises a step_failed decision (routed via handleStepFailure);
	// s2 must stay pending while s1 is blocked on the decision.
	waitUntil(t, 20*time.Second, "step_failed decision never raised", func() bool {
		return pendingDecisionOfType(t, engine, taskID, "step_failed") != nil
	})
	dec := pendingDecisionOfType(t, engine, taskID, "step_failed")
	decID := fmt.Sprint(dec["id"])
	if decID == "" || decID == "<nil>" {
		t.Fatalf("step_failed decision has no id: %v", dec)
	}
	hasRetry := false
	switch opts := dec["options"].(type) {
	case []string: // in-process shape
		for _, o := range opts {
			if o == "retry" {
				hasRetry = true
			}
		}
	case []any: // post-JSON shape
		for _, o := range opts {
			if fmt.Sprint(o) == "retry" {
				hasRetry = true
			}
		}
	}
	if !hasRetry {
		t.Errorf("step_failed decision options lack retry: %v", dec["options"])
	}
	if got := stepStatus(t, engine, taskID, "s2"); got != "pending" {
		t.Errorf("s2 = %q while s1 is blocked on the failure decision, want pending", got)
	}

	// Human picks retry → s1 re-queued → walker relaunches → s1 ok → s2 runs.
	if err := engine.SubmitDecision(taskID, decID, "retry"); err != nil {
		t.Fatalf("SubmitDecision(retry): %v", err)
	}

	st, ok := engine.WaitTask(ctx, taskID, 30*time.Second, "full")
	if !ok {
		t.Fatalf("WaitTask returned not-found")
	}
	if gs := fmt.Sprint(st["status"]); gs != "completed" {
		t.Errorf("graph status = %q, want completed after retry; full=%v", gs, st)
	}
	if got := stepStatus(t, engine, taskID, "s1"); got != "ok" {
		t.Errorf("terminal: s1 = %q, want ok after retry", got)
	}
	if got := stepStatus(t, engine, taskID, "s2"); got != "ok" {
		t.Errorf("terminal: s2 = %q, want ok (ran after s1's retry)", got)
	}
	if decs, _ := st["pending_decisions"].([]map[string]any); len(decs) != 0 {
		t.Errorf("pending decisions remain after resolution: %v", decs)
	}
	if calls := p.callCount(); calls != 6 {
		t.Errorf("provider calls = %d, want exactly 6 (s1 tool+fail, retry tool+ok, s2 tool+ok)", calls)
	}
}
