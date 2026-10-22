package core

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// Regression for A7 (eval FINDINGS.md §A7, 2026-10-22): DisableForceTool is a
// single switch that used to gate TWO mechanisms — the provider-side
// forced-tool output constraint (:323) and this prompt-side no-tool nudge
// (:817). Disabling the constraint ("give the model freedom") silently
// silenced the reminder ("a motionless model gets told to act"), so a
// deployment setting disable_force_tool=true lost spin protection entirely.
// The nudge no longer consults the flag; the provider-side constraint still
// does. Not triggered in the v20 runs (flag absent), so this pins the
// decoupling for the first deployment that sets it.

// forceToolFreeProvider replays the A7 scenario under
// DisableForceTool=true: prose with no tool call and no JSON (turn 0), then a
// read-only domain call (turn 1), then a valid ok JSON (turn 2). It counts
// provider calls: without the fix the turn-0 nudge never fires and the
// degraded partial is accepted after ONE call.
type forceToolFreeProvider struct {
	calls atomic.Int64
}

func (p *forceToolFreeProvider) Name() string { return "mock-ftf" }

func (p *forceToolFreeProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *forceToolFreeProvider) respond() *schemas.ProviderResponse {
	switch p.calls.Load() {
	case 1:
		return &schemas.ProviderResponse{Text: "Plain prose, no JSON structure at all, no tool call."}
	case 2:
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{
			{Name: "venmo__show_transactions", Arguments: map[string]any{"limit": 1}},
		}}
	default:
		return &schemas.ProviderResponse{Text: `{"status":"ok","confidence":0.9,"result":{"summary":"done"}}`}
	}
}

func (p *forceToolFreeProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return p.respond(), nil
}

func (p *forceToolFreeProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return p.respond(), nil
}

func (p *forceToolFreeProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}

// TestNoToolNudge_FiresDespiteDisableForceTool drives the real spawn loop
// with DisableForceTool=true and asserts the turn-0 no-tool nudge still
// fires: the provider is called three times (prose → nudged → read-only call
// → ok JSON) instead of the step being accepted as a degraded partial after
// a single call.
func TestNoToolNudge_FiresDespiteDisableForceTool(t *testing.T) {
	engine := newSpinLoopEngine(t)

	engine.Mu.Lock()
	engine.registry.System.FeatureFlags.DisableForceTool = true
	engine.Mu.Unlock()

	prov := &forceToolFreeProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-ftf",
		BaseURL:  "mock://ftf",
		Model:    "ftf-1",
	}
	registerSpinProvider(t, cfg, prov)

	engine.Mu.Lock()
	engine.registry.Providers["ftf"] = cfg
	engine.registry.DefaultProvider = "ftf"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "ftf"
	}
	engine.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := engine.spawner.Spawn(ctx, &SpawnRequest{
		TaskID: "ftf_task_1",
		StepID: "ftf_step_1",
		RoleID: "worker",
		Task:   "list my recent transactions",
	})
	if err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	if res == nil {
		t.Fatal("Spawn returned nil result")
	}

	calls := prov.calls.Load()
	t.Logf("[ftf-branch] provider_calls=%d status=%v conf=%.2f warnings=%v",
		calls, res.Output.Status, res.Output.Confidence, res.Output.Warnings)

	// A7 REGRESSION: before the fix the turn-0 nudge was gated by
	// !DisableForceTool, so with the flag set the degraded partial was
	// accepted after a single provider call.
	if calls < 3 {
		t.Errorf("A7 REGRESSION: nudge silenced by DisableForceTool — loop accepted the degraded "+
			"output after %d call(s) (status=%v conf=%.2f); expected the spin to be broken by the "+
			"no-tool nudge (3+ calls)", calls, res.Output.Status, res.Output.Confidence)
	}
	if res.Output.Status != schemas.StatusOK {
		t.Errorf("A7 REGRESSION: expected recovered ok status, got %v", res.Output.Status)
	}
	if strings.Contains(strings.Join(res.Output.Warnings, "; "), "broke spin loop") {
		t.Logf("[ftf-branch] note: spin breaker fired (acceptable — nudge and breaker may both act); calls=%d", calls)
	}
}
