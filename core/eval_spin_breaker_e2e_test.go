package core

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// spinGarbageProvider always answers with prose that ParseOutput cannot turn
// into a structured subagent output. It counts how many times the spawner's
// tool loop actually called it, which is the number we assert on: without the
// P1 spin breaker the loop burns the whole turn budget on identical garbage.
type spinGarbageProvider struct{ calls atomic.Int64 }

const spinGarbageText = "This is plain prose. It carries no tool call and no valid status/confidence/result JSON."

func (p *spinGarbageProvider) Name() string { return "mock-spin-garbage" }

func (p *spinGarbageProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *spinGarbageProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return &schemas.ProviderResponse{Text: spinGarbageText}, nil
}

func (p *spinGarbageProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return &schemas.ProviderResponse{Text: spinGarbageText}, nil
}

func (p *spinGarbageProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }

// newSpinLoopEngine builds an engine whose tool loop is actually reachable.
// It deliberately leaves System.DelegationMode false: when delegation mode is
// on, tryDelegationMode short-circuits every spawn and returns
// StatusDelegationRequired, so the P1 spin breaker inside the tool loop is
// never entered and could never be observed from a test.
func newSpinLoopEngine(t *testing.T) *DirectedEngine {
	t.Helper()
	providers.ClearCache()

	reg := &config.Registry{
		DefaultProvider: "garbage",
		Providers:       map[string]*config.ProviderConfig{},
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		System: config.SystemSettings{ConfidenceThreshold: 0.7},
	}

	logDir := t.TempDir()
	outDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	expDir := t.TempDir()
	expStore := mustNewExperienceStore(t, expDir, ts, &reg.System, nil, nil)
	jitDir := t.TempDir()
	jit := NewJITManager(reg, jitDir)

	engine := NewDirectedEngine(reg, ts, expStore, jit, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { engine.Stop() })
	return engine
}

// registerSpinProvider injects a mock provider into the global router under a
// key that matches config.ProviderConfig's cache key, which is exactly how
// providers.Get resolves pre-registered test mocks.
func registerSpinProvider(t *testing.T, cfg *config.ProviderConfig, p providers.Provider) {
	t.Helper()
	slotKey := providers.PoolIDFor(cfg) // == providerCacheKey(cfg) when PoolID is unset
	providers.GlobalRouter.Register(slotKey, cfg, p)
	t.Cleanup(func() { providers.GlobalRouter.Deregister(slotKey) })
}

// TestEvalS05_SpinBreakerEndToEndBreaksOnParseFailures closes the gap left by
// the existing P1 tests: they only asserted the ParseOutput *assumption* side
// ("not parseable" is produced), never that the spawner's tool loop actually
// breaks. This drives the real tool loop with a permanently unparseable
// provider and asserts the loop stopped early and reported the break.
func TestEvalS05_SpinBreakerEndToEndBreaksOnParseFailures(t *testing.T) {
	engine := newSpinLoopEngine(t)

	garbage := &spinGarbageProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-spin-garbage",
		BaseURL:  "mock://spin-garbage",
		Model:    "garbage-1",
	}
	registerSpinProvider(t, cfg, garbage)

	// Single provider candidate, so any call count above the breaker threshold
	// is the spin breaker failing rather than provider failover.
	engine.Mu.Lock()
	engine.registry.Providers["garbage"] = cfg
	engine.registry.DefaultProvider = "garbage"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "garbage"
	}
	engine.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := engine.spawner.Spawn(ctx, &SpawnRequest{
		TaskID: "spin_task_1",
		StepID: "spin_step_1",
		RoleID: "worker",
		Task:   "summarize the customer complaint and propose a refund",
	})
	calls := garbage.calls.Load()
	// res may be nil when Spawn returns an error, so guard before logging.
	if res != nil {
		t.Logf("[spin-branch] provider_calls=%d status=%v warnings=%v err=%v",
			calls, res.Output.Status, res.Output.Warnings, err)
	} else {
		t.Logf("[spin-branch] provider_calls=%d err=%v (no SpawnResult)", calls, err)
	}

	// Baseline: confirm the exact text the loop parses does trip the detector.
	probe := schemas.ParseOutput(spinGarbageText, "text")
	t.Logf("[spin-branch] ParseOutput Assumptions=%v Status=%v Confidence=%v",
		probe.Assumptions, probe.Status, probe.Confidence)

	if calls == 0 {
		t.Fatalf("P1 REGRESSION: provider never called — mock injection never reached the tool loop, " +
			"so this test proves nothing")
	}
	// The breaker fires after maxConsecutiveParseFailures=3 provider calls,
	// so anything well above that means it stopped protecting the loop.
	if calls > 6 {
		t.Errorf("P1 REGRESSION: spin breaker did not fire; burned %d provider calls on identical "+
			"unparseable output", calls)
	}

	// P1 graceful degradation (2026-10-03): the spin breaker now returns a
	// partial result directly instead of falling through to ErrMaxTurnsExhausted.
	// Resuming won't help — the model will keep producing unparseable output.
	// Regression: if this reverts, the caller sees a hard failure instead of a
	// usable partial result, and the "broke spin loop" warning is lost.
	if err != nil {
		t.Fatalf("P1 REGRESSION: spin breaker should return partial result, got error: %v", err)
	}
	if res == nil {
		t.Fatal("P1 REGRESSION: spin breaker should return non-nil result")
	}
	for _, w := range res.Output.Warnings {
		if strings.Contains(w, "broke spin loop") {
			t.Logf("[spin-branch] spin-break warning present in output (status=%v, turns=%d)",
				res.Output.Status, res.TurnsUsed)
			return
		}
	}
	t.Errorf("P1 REGRESSION: no 'broke spin loop' warning in output (provider_calls=%d, warnings=%v)",
		calls, res.Output.Warnings)
}

// TestEvalS05_SpinBreakerStaysQuietOnParseableOutput is the control case: a
// provider that returns parseable output must NOT be cut short by the spin
// breaker, otherwise the breaker would be over-firing and hiding real work.
func TestEvalS05_SpinBreakerStaysQuietOnParseableOutput(t *testing.T) {
	engine := newSpinLoopEngine(t)

	good := &spinGoodProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-spin-good",
		BaseURL:  "mock://spin-good",
		Model:    "good-1",
	}
	registerSpinProvider(t, cfg, good)

	engine.Mu.Lock()
	engine.registry.Providers["good"] = cfg
	engine.registry.DefaultProvider = "good"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "good"
	}
	engine.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := engine.spawner.Spawn(ctx, &SpawnRequest{
		TaskID: "spin_task_2",
		StepID: "spin_step_1",
		RoleID: "worker",
		Task:   "summarize the customer complaint and propose a refund",
	})
	if err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}

	calls := good.calls.Load()
	t.Logf("[spin-branch] provider_calls=%d status=%v warnings=%v", calls, res.Output.Status, res.Output.Warnings)

	for _, w := range res.Output.Warnings {
		if strings.Contains(w, "broke spin loop") {
			t.Errorf("P1 REGRESSION: spin breaker fired on parseable output (call it not spinning) — %v", w)
		}
	}
}

// spinGoodProvider returns valid structured output and stops the loop on its
// own after the first turn.
type spinGoodProvider struct{ calls atomic.Int64 }

func (p *spinGoodProvider) Name() string { return "mock-spin-good" }

func (p *spinGoodProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *spinGoodProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return &schemas.ProviderResponse{Text: `{"status":"ok","confidence":0.9,"result":{"summary":"customer was delayed and wants a refund"}}`}, nil
}

func (p *spinGoodProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	return &schemas.ProviderResponse{Text: `{"status":"ok","confidence":0.9,"result":{"summary":"customer was delayed and wants a refund"}}`}, nil
}

func (p *spinGoodProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
