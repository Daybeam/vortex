package core

// fastpath_approval_gate_test.go — Regression test for the audit finding that
// the single-step fast path swallowed StatusApprovalRequired into StepFailed +
// GraphFailed instead of raising a human_approval_required decision (G2).
//
// Before the fix (scheduler_submit.go:584 switch), StatusApprovalRequired fell
// into the `default` case which set StepFailed + GraphFailed — a mount-policy
// `approval` MCP silently failed instead of surfacing an approval gate.
//
// After the fix, an explicit `case schemas.StatusApprovalRequired:` sets
// StepBlocked and raises a DecisionHumanApprovalRequired via
// emitMountApprovalDecision, mirroring the walker path
// (handleStepFailure → diagnoseFault → emitMountApprovalDecision).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/jit"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// approvalProvider returns StatusApprovalRequired with a mount that needs
// human approval. No tool calls on purpose: every semantic gate is
// short-circuited by noToolCallsYet, so the turn ladder is deterministic.
type approvalProvider struct {
	calls atomic.Int64
}

func (p *approvalProvider) Name() string { return "mock-approval" }

func (p *approvalProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *approvalProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}

func (p *approvalProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return &schemas.ProviderResponse{
		Text: `{"status":"approval_required","confidence":0.8,` +
			`"result":"MCP mount needs human approval",` +
			`"approval_required_mounts":["sensitive_db"]}`,
		StopReason: "end_turn",
	}, nil
}

func (p *approvalProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	return p.Complete(context.Background(), schemas.CompleteRequest{})
}

func TestFastPath_ApprovalRequired_RaisesHumanApprovalDecision(t *testing.T) {
	providers.ClearCache()
	t.Cleanup(providers.ClearCache)

	logDir := t.TempDir()
	outDir := t.TempDir()

	reg := &config.Registry{
		DefaultProvider: "approval",
		Providers:       map[string]*config.ProviderConfig{},
		Roles: map[string]*config.Role{
			"worker": {
				ID:             "worker",
				BaseCapability: "text",
				Provider:       "approval",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
			MaxToolTurns:        2,
			FeatureFlags: config.FeatureFlags{
				EvalObs:            true,
				DisablePatternKill: true,
			},
		},
	}

	logger := mustNewLogger(t, logDir, &reg.System)
	ts := store.NewTaskStore(&mockBackend{data: make(map[string][]byte)})
	expStore := mustNewExperienceStore(t, t.TempDir(), ts, &reg.System, nil, nil)
	jitMgr := jit.NewJITManager(reg, t.TempDir())

	engine := NewDirectedEngine(reg, ts, expStore, jitMgr, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })

	stub := &approvalProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-approval",
		BaseURL:  "mock://approval",
		Model:    "approval-1",
	}
	slotKey := providers.PoolIDFor(cfg)
	providers.GlobalRouter.Register(slotKey, cfg, stub)
	t.Cleanup(func() { providers.GlobalRouter.Deregister(slotKey) })
	reg.Providers["approval"] = cfg

	taskID, err := engine.Submit([]schemas.StepInput{{
		ID:     "s1",
		RoleID: "worker",
		Task:   "query the sensitive database",
	}})
	if err != nil {
		t.Fatalf("Submit rejected the single-step task: %v", err)
	}

	// Gate 1: a human_approval_required decision must be raised
	got := waitFor(30*time.Second, func() bool {
		return engine.PendingDecisionCount(taskID) > 0
	})
	if !got {
		var status schemas.GraphStatus
		var stepStatus schemas.StepStatus
		engine.WithTaskGraph(taskID, func(g *schemas.TaskGraph) {
			status = g.Status
			for _, st := range g.Steps {
				stepStatus = st.Status
			}
		})
		t.Fatalf("no decision was raised within 30s — StatusApprovalRequired "+
			"was swallowed instead of raising G2. graph=%s step=%s calls=%d",
			status, stepStatus, stub.calls.Load())
	}

	var graphStatus schemas.GraphStatus
	var stepStatus schemas.StepStatus
	var dec schemas.Decision
	engine.WithTaskGraph(taskID, func(g *schemas.TaskGraph) {
		graphStatus = g.Status
		for _, st := range g.Steps {
			stepStatus = st.Status
		}
		if len(g.PendingDecisions) > 0 {
			dec = *g.PendingDecisions[0]
		}
	})

	// Step must be blocked, not failed
	if stepStatus != schemas.StepBlocked {
		t.Errorf("step status = %s, want %s — approval must block, not fail",
			stepStatus, schemas.StepBlocked)
	}

	// Graph must still be running, not failed
	if graphStatus == schemas.GraphFailed {
		t.Errorf("graph status = %s — approval must not fail the graph",
			graphStatus)
	}

	// Decision must be human_approval_required
	if dec.Type != schemas.DecisionHumanApprovalRequired {
		t.Errorf("decision type = %q, want %q",
			dec.Type, schemas.DecisionHumanApprovalRequired)
	}

	// Options must include approve and reject
	if !containsString(dec.Options, "approve") {
		t.Errorf("options %v must include approve", dec.Options)
	}
	if !containsString(dec.Options, "reject") {
		t.Errorf("options %v must include reject", dec.Options)
	}

	t.Logf("PASS: approval decision raised — graph=%s step=%s decision=%s options=%v",
		graphStatus, stepStatus, dec.Type, dec.Options)
}
