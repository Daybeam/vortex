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

// Regression for A5 (eval FINDINGS.md §A5, 2026-10-22): the mid-abandonment
// blind spot shared by all three completion gates. The model starts working
// (read-only calls land in the trace → noToolCallsYet=false), hits friction,
// then emits a PARSEABLE partial and stops while turns remain. Before the
// gate: the partial was accepted as-is right after the gates (1 tool call +
// 1 give-up turn = step done). The v20 shape: "调了 3 次工具、遇到一次 422、
// 然后停手" — three reads, one 422, then giving up, with every gate silent.

// midAbandonProvider replays the exact shape:
//
//	turn 0 — one read-only domain call (lands in trace);
//	turn 1 — PARSEABLE partial JSON ("worked to the middle, gave up");
//	turn 2 — valid ok JSON (recovery after the nudge).
//
// It snapshots the last flattened user context so the test can assert the
// nudge actually reached the model.
type midAbandonProvider struct {
	calls    atomic.Int64
	lastUser atomic.Value // string
}

func (p *midAbandonProvider) Name() string { return "mock-mid-abandon" }

func (p *midAbandonProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *midAbandonProvider) respond() *schemas.ProviderResponse {
	switch p.calls.Load() {
	case 1:
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{
			{Name: "venmo__show_transactions", Arguments: map[string]any{"limit": 1}},
		}}
	case 2:
		// Parseable, clean partial — NOT parseFailed. Every pre-A5 gate is
		// blind to exactly this shape.
		return &schemas.ProviderResponse{Text: `{"status":"partial","confidence":0.5,"result":{"reply":"I could not find the recurring transaction."}}`}
	default:
		return &schemas.ProviderResponse{Text: `{"status":"ok","confidence":0.9,"result":{"summary":"done"}}`}
	}
}

func (p *midAbandonProvider) Complete(_ context.Context, r schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	p.lastUser.Store(r.User)
	return p.respond(), nil
}

func (p *midAbandonProvider) StreamComplete(_ context.Context, r schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	p.lastUser.Store(r.User)
	return p.respond(), nil
}

func (p *midAbandonProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}

// TestMidAbandonGate_NudgesParseablePartialAfterReadOnlyWork asserts that
// with mid_abandon_nudge=true the give-up turn is intercepted: the nudge
// reaches the model's next-turn context and the loop continues to the
// recovery turn instead of accepting the partial.
func TestMidAbandonGate_NudgesParseablePartialAfterReadOnlyWork(t *testing.T) {
	engine := newSpinLoopEngine(t)

	engine.Mu.Lock()
	engine.registry.System.FeatureFlags.MidAbandonNudge = true
	engine.Mu.Unlock()

	prov := &midAbandonProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-mid-abandon",
		BaseURL:  "mock://mid-abandon",
		Model:    "abandon-1",
	}
	registerSpinProvider(t, cfg, prov)

	engine.Mu.Lock()
	engine.registry.Providers["abandon"] = cfg
	engine.registry.DefaultProvider = "abandon"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "abandon"
	}
	engine.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := engine.spawner.Spawn(ctx, &SpawnRequest{
		TaskID: "ma_task_1",
		StepID: "ma_step_1",
		RoleID: "worker",
		Task:   "list my recent transactions and cancel the recurring one",
	})
	if err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	if res == nil {
		t.Fatal("Spawn returned nil result")
	}

	calls := prov.calls.Load()
	lastUser, _ := prov.lastUser.Load().(string)
	t.Logf("[ma-branch] provider_calls=%d status=%v conf=%.2f", calls, res.Output.Status, res.Output.Confidence)

	// The recovery turn only happens if the loop CONTINUED past the clean
	// partial instead of accepting it.
	if calls < 3 {
		t.Errorf("A5 REGRESSION: loop accepted the parseable partial after %d provider calls "+
			"(status=%v conf=%.2f) — mid-abandonment was silent", calls, res.Output.Status, res.Output.Confidence)
	}
	if !strings.Contains(lastUser, SpawnerMidAbandonMsg[:60]) {
		t.Errorf("A5 REGRESSION: mid-abandonment message never reached the model context; "+
			"last user context tail: %.300s", lastUser)
	}
	if res.Output.Status != schemas.StatusOK {
		t.Errorf("A5 REGRESSION: expected recovered ok status, got %v (conf %.2f)", res.Output.Status, res.Output.Confidence)
	}
}

// TestMidAbandonGate_QuietWhenWorkHasEffect is the control: a trace that
// already contains a state-changing call means the partial is a genuine
// "did real work, needs one more thing" — the gate must NOT fire and nag.
func TestMidAbandonGate_QuietWhenWorkHasEffect(t *testing.T) {
	engine := newSpinLoopEngine(t)

	engine.Mu.Lock()
	engine.registry.System.FeatureFlags.MidAbandonNudge = true
	engine.Mu.Unlock()

	prov := &midAbandonEffectProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-ma-effect",
		BaseURL:  "mock://ma-effect",
		Model:    "effect-1",
	}
	registerSpinProvider(t, cfg, prov)

	engine.Mu.Lock()
	engine.registry.Providers["maeffect"] = cfg
	engine.registry.DefaultProvider = "maeffect"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "maeffect"
	}
	engine.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := engine.spawner.Spawn(ctx, &SpawnRequest{
		TaskID: "ma_task_2",
		StepID: "ma_step_1",
		RoleID: "worker",
		Task:   "cancel the recurring transaction",
	})
	if err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}

	lastUser, _ := prov.lastUser.Load().(string)
	t.Logf("[ma-ctl] provider_calls=%d status=%v", prov.calls.Load(), res.Output.Status)

	if strings.Contains(lastUser, SpawnerMidAbandonMsg[:60]) {
		t.Error("A5 OVER-FIRE: mid-abandonment message fired although the trace contained a state-changing call")
	}
}

// midAbandonEffectProvider: turn 0 a WRITE call (lands in trace, fails
// harmlessly — no MCP registered), turn 1 a clean partial. traceHasEffect is
// true → the gate must stay quiet and the partial is accepted as-is.
type midAbandonEffectProvider struct {
	calls    atomic.Int64
	lastUser atomic.Value // string
}

func (p *midAbandonEffectProvider) Name() string { return "mock-ma-effect" }

func (p *midAbandonEffectProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *midAbandonEffectProvider) respond() *schemas.ProviderResponse {
	switch p.calls.Load() {
	case 1:
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{
			{Name: "venmo__cancel_payment", Arguments: map[string]any{"payment_id": "P-1"}},
		}}
	default:
		return &schemas.ProviderResponse{Text: `{"status":"partial","confidence":0.5,"result":{"reply":"waiting for confirmation code"}}`}
	}
}

func (p *midAbandonEffectProvider) Complete(_ context.Context, r schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	p.lastUser.Store(r.User)
	return p.respond(), nil
}

func (p *midAbandonEffectProvider) StreamComplete(_ context.Context, r schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	p.lastUser.Store(r.User)
	return p.respond(), nil
}

func (p *midAbandonEffectProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}
