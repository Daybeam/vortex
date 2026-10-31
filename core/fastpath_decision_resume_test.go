package core

// ─────────────────────────────────────────────────────────────────────────────
// T1: the single-step fast path raises a delegation decision — does the graph
// reach a terminal state after the caller answers?
//
// WHY THIS FILE EXISTS (2026-10-31)
//
// Adapted from the original aw20 stall test (e66e46abe). The original tested
// the `partialBlocked` branch which has since been removed — partial status now
// sets GraphFailed directly (the "honest terminal" approach, scheduler_submit.go
// :568-582). This file instead covers the `delegationBlocked` branch, which is
// the remaining decision-raising path in the fast path.
//
// The delegation branch raises a DecisionDelegationRequired and returns. For
// single-step (IsSmartRouted) graphs, finalizeSmartRoutedAfterDecision handles
// terminal finalization inline after the decision is resolved — no run() loop
// is strictly needed. The startGraphExecutor call (C5DD 2026-10-29) is
// defense-in-depth: it starts a run() loop that parks on the notify channel
// and is harmlessly no-op'd by the idempotency guard in finalize() if
// finalizeSmartRoutedAfterDecision already finalized.
//
// This test verifies the end-to-end flow: submit → delegation decision →
// skip → terminal. It would fail if the delegation branch were removed
// without a replacement, or if finalizeSmartRoutedAfterDecision regressed.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// delegationProvider plays a subagent that answers with well-formed JSON and
// declares `status: "delegation_required"`.
//
// No tool calls on purpose: every semantic gate is short-circuited by
// noToolCallsYet, so the turn ladder is deterministic.
type delegationProvider struct {
	calls atomic.Int64
}

func (p *delegationProvider) Name() string { return "mock-delegation" }

func (p *delegationProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *delegationProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}

func (p *delegationProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.next(), nil
}

func (p *delegationProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	return p.next(), nil
}

func (p *delegationProvider) next() *schemas.ProviderResponse {
	n := int(p.calls.Add(1))
	return &schemas.ProviderResponse{
		Text: fmt.Sprintf(
			`{"status":"delegation_required","confidence":0.6,`+
				`"result":{"attempt":%d,"note":"need human input to proceed"},`+
				`"missing_context":["external data not available"]}`, n),
		StopReason: "end_turn",
	}
}

// ── harness ──────────────────────────────────────────────────────────────────

type fastPathHarness struct {
	engine   *DirectedEngine
	reg      *config.Registry
	provider *delegationProvider
}

// newFastPathHarness builds an engine that reaches the real fast path:
//
//   - one dependency-free step, so `len(inputs)==1 && len(DependsOn)==0`
//     (scheduler_submit.go:283) is satisfied — the only entry to the code under
//     test;
//   - RequirePlanReview false and enable_planner_first unset, so the submission
//     is not rewritten into a DAG by the planner before the fast path sees it;
//   - a role with no MCP bindings, so no subprocess is spawned and the test has
//     no python/network dependency;
//   - MaxToolTurns=2, which is the smallest budget that still lets the
//     no-tool-call nudge fire once and then return a verdict.
func newFastPathHarness(t *testing.T, maxTurns int) *fastPathHarness {
	t.Helper()
	providers.ClearCache()

	logDir := t.TempDir()
	outDir := t.TempDir()

	reg := &config.Registry{
		DefaultProvider: "delegation",
		Providers:       map[string]*config.ProviderConfig{},
		Roles: map[string]*config.Role{
			"worker": {
				ID:             "worker",
				BaseCapability: "text",
				Provider:       "delegation",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			MaxToolTurns:        maxTurns,
			FeatureFlags: config.FeatureFlags{
				EvalObs:           true,
				DisablePatternKill: true,
			},
		},
	}

	logger := mustNewLogger(t, logDir, &reg.System)
	ts := store.NewTaskStore(&mockBackend{data: make(map[string][]byte)})
	expStore := mustNewExperienceStore(t, t.TempDir(), ts, &reg.System, nil, nil)
	jit := NewJITManager(reg, t.TempDir())

	engine := NewDirectedEngine(reg, ts, expStore, jit, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })

	stub := &delegationProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-delegation",
		BaseURL:  "mock://delegation",
		Model:    "delegation-1",
	}
	slotKey := providers.PoolIDFor(cfg)
	providers.GlobalRouter.Register(slotKey, cfg, stub)
	t.Cleanup(func() { providers.GlobalRouter.Deregister(slotKey) })
	reg.Providers["delegation"] = cfg

	return &fastPathHarness{engine: engine, reg: reg, provider: stub}
}

// snapshot copies the state the assertions need, under the engine lock.
type graphSnapshot struct {
	graphStatus schemas.GraphStatus
	stepStatus  schemas.StepStatus
	stepErr     string
	decisions   []schemas.Decision
}

func (h *fastPathHarness) snapshot(taskID string) graphSnapshot {
	h.engine.Mu.RLock()
	defer h.engine.Mu.RUnlock()
	g := h.engine.graphs[taskID]
	if g == nil {
		return graphSnapshot{}
	}
	var snap graphSnapshot
	snap.graphStatus = g.Status
	for _, st := range g.Steps {
		snap.stepStatus = st.Status
		snap.stepErr = st.LastError
	}
	for _, d := range g.PendingDecisions {
		snap.decisions = append(snap.decisions, *d)
	}
	return snap
}

func (h *fastPathHarness) describe(taskID string) string {
	snap := h.snapshot(taskID)
	ids := make([]string, 0, len(snap.decisions))
	for _, d := range snap.decisions {
		ids = append(ids, string(d.Type)+"/"+d.ID)
	}
	return fmt.Sprintf("graph=%s step=%s step_err=%q decisions=[%s] provider_calls=%d",
		snap.graphStatus, snap.stepStatus, snap.stepErr, strings.Join(ids, " "), h.provider.calls.Load())
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// ── the criterion ────────────────────────────────────────────────────────────

func TestFastPath_DelegationDecisionSkipReachesTerminal(t *testing.T) {
	const maxTurns = 2
	h := newFastPathHarness(t, maxTurns)

	taskID, err := h.engine.Submit([]schemas.StepInput{{
		ID:     "s1",
		RoleID: "worker",
		Task:   "reconcile the three invoices against the ledger",
	}})
	if err != nil {
		t.Fatalf("Submit rejected the single-step task: %v", err)
	}
	t.Logf("[t1] task=%s", taskID)

	// ── Gate 1: the fast path raised a delegation decision ───────────────
	got := waitFor(30*time.Second, func() bool {
		return len(h.snapshot(taskID).decisions) > 0
	})
	if !got {
		t.Fatalf("no decision was raised within 30s — the delegation verdict "+
			"never reached addDecision. State: %s", h.describe(taskID))
	}

	snap := h.snapshot(taskID)
	if snap.graphStatus != schemas.GraphRunning {
		t.Fatalf("graph is %s, want running — a delegation must PAUSE the "+
			"task, not finish it. State: %s", snap.graphStatus, h.describe(taskID))
	}
	if snap.stepStatus != schemas.StepBlocked {
		t.Fatalf("step is %s, want blocked. State: %s", snap.stepStatus, h.describe(taskID))
	}
	dec := snap.decisions[0]
	if dec.Type != schemas.DecisionDelegationRequired {
		t.Fatalf("decision type is %q, want %q. State: %s",
			dec.Type, schemas.DecisionDelegationRequired, h.describe(taskID))
	}
	if !containsString(dec.Options, "skip") {
		t.Fatalf("options %v do not offer skip. State: %s", dec.Options, h.describe(taskID))
	}
	t.Logf("[t1] delegation decision raised: %s", h.describe(taskID))

	// ── Gate 2: answering "skip" must reach a terminal state ────────────
	// This verifies the full flow: SubmitDecision(skip) → step=Skipped →
	// finalizeSmartRoutedAfterDecision → GraphCompletedWithSkips.
	// Without finalizeSmartRoutedAfterDecision, the graph would stay
	// "running" forever (the original aw20 stall pattern).
	if err := h.engine.SubmitDecision(taskID, dec.ID, "skip"); err != nil {
		t.Fatalf("SubmitDecision(skip) failed: %v", err)
	}

	terminal := waitFor(30*time.Second, func() bool {
		s := h.snapshot(taskID)
		return s.graphStatus == schemas.GraphCompletedWithSkips ||
			s.graphStatus == schemas.GraphCompleted ||
			s.graphStatus == schemas.GraphFailed
	})
	if !terminal {
		t.Fatalf("graph never reached terminal state after skip — "+
			"finalizeSmartRoutedAfterDecision did not fire. State: %s",
			h.describe(taskID))
	}

	finalSnap := h.snapshot(taskID)
	if finalSnap.graphStatus != schemas.GraphCompletedWithSkips {
		t.Fatalf("graph is %s, want GraphCompletedWithSkips — a skipped "+
			"step on a single-step graph should complete-with-skips. State: %s",
			finalSnap.graphStatus, h.describe(taskID))
	}
	if len(finalSnap.decisions) != 0 {
		t.Fatalf("decisions should be empty after resolution, got %d. State: %s",
			len(finalSnap.decisions), h.describe(taskID))
	}
	t.Logf("[t1] PASS: %s", h.describe(taskID))
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
