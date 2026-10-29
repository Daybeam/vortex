package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// PREREG-CTX51 (铁律 51 的四条修法). Deterministic, no Kaggle, no network.
//
// D1 块结构守恒 / D2 预算裁剪 / D3 事件可观测 / D4 闸门算入 MaxTokens.

// ---------------------------------------------------------------------------
// stub provider
// ---------------------------------------------------------------------------

// ctx51Stub is a Provider that counts tokens as len(text) so fixtures can pin
// exact numbers, and can fail its first N StreamComplete calls with a given
// error (used to inject the real llama.cpp 400).
type ctx51Stub struct {
	name string

	failFirst int
	failErr   error

	mu         sync.Mutex
	calls      int
	userBlocks int // len(req.UserBlocks) as seen on the LAST call
	sysBlocks  int
	// promptTokens is the estimated size of the prompt the provider was handed
	// (blocks are what actually gets serialized).
	promptTokens int
	lastUser     []schemas.ContentBlock
	lastSystem   []schemas.ContentBlock
	// perToken: 0 or 1 => count bytes (D4 pins exact numbers this way);
	// 4 => mimic ~4 chars/token so Audit sees token-like magnitudes (D3).
	perToken int
}

func (p *ctx51Stub) Name() string { return p.name }

func (p *ctx51Stub) Embed(context.Context, string) ([]float32, error) { return nil, nil }

func (p *ctx51Stub) CountTokens(_ context.Context, s string) (int, error) {
	if p.perToken > 1 {
		return (len(s) + p.perToken - 1) / p.perToken, nil
	}
	return len(s), nil
}

func (p *ctx51Stub) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, nil)
}

func (p *ctx51Stub) StreamComplete(_ context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.userBlocks = len(req.UserBlocks)
	p.sysBlocks = len(req.SystemBlocks)
	p.promptTokens = estimateBlocksTokens(req.UserBlocks) + estimateBlocksTokens(req.SystemBlocks)
	p.lastUser = req.UserBlocks
	p.lastSystem = req.SystemBlocks
	p.mu.Unlock()

	if n <= p.failFirst {
		return nil, p.failErr
	}
	if onChunk != nil {
		_ = onChunk("ok")
	}
	return &schemas.ProviderResponse{Text: "ok", CompletionTokens: 1}, nil
}

func (p *ctx51Stub) stats() (calls, userBlocks, sysBlocks int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.userBlocks, p.sysBlocks
}

// ---------------------------------------------------------------------------
// D1 — Squeeze must preserve block granularity (铁律 51-1)
// ---------------------------------------------------------------------------

func TestCTX51_D1_SqueezePreservesBlockStructure(t *testing.T) {
	stub := &ctx51Stub{name: "ctx51-openai"}
	reg := &config.Registry{System: config.SystemSettings{MaxContextKeep: 10}}
	cm := NewContextManager(reg)

	sys := make([]schemas.ContentBlock, 3)
	for i := range sys {
		sys[i] = schemas.ContentBlock{Type: "text", Text: fmt.Sprintf("sys-%d %s", i, strings.Repeat("a", 160))}
	}
	usr := make([]schemas.ContentBlock, 7)
	for i := range usr {
		usr[i] = schemas.ContentBlock{Type: "text", Text: fmt.Sprintf("usr-%d %s", i, strings.Repeat("b", 160))}
	}
	prot := []schemas.ContentBlock{{Type: "text", Text: "PROTECTED-MUST-SURVIVE"}}
	req := &schemas.CompleteRequest{
		SystemBlocks:        sys,
		UserBlocks:          usr,
		ProtectedUserBlocks: prot,
		Model:               "m",
		MaxTokens:           100,
	}

	got, _, err := cm.Squeeze(context.Background(), stub, req, LevelAggressive)
	if err != nil {
		t.Fatalf("Squeeze returned error: %v", err)
	}
	if got == nil {
		t.Fatal("Squeeze returned nil request")
	}

	// The whole point of F1: granularity survives compression. Before the fix
	// both slices were nil and the caller collapsed them into ONE block.
	if len(got.UserBlocks) != len(usr) {
		t.Errorf("UserBlocks granularity lost: got %d, want %d", len(got.UserBlocks), len(usr))
	}
	if len(got.SystemBlocks) != len(sys) {
		t.Errorf("SystemBlocks granularity lost: got %d, want %d", len(got.SystemBlocks), len(sys))
	}
	// Flat fallbacks must stay populated.
	if got.System == "" || got.User == "" {
		t.Errorf("flat fallback strings emptied: sys=%d bytes user=%d bytes", len(got.System), len(got.User))
	}
	// Protected zone: carried verbatim in its own field, NOT duplicated into the
	// volatile slices (would double-bill wherever both are concatenated).
	if len(got.ProtectedUserBlocks) != 1 || got.ProtectedUserBlocks[0].Text != "PROTECTED-MUST-SURVIVE" {
		t.Errorf("protected zone not carried verbatim: %+v", got.ProtectedUserBlocks)
	}
	for _, b := range got.UserBlocks {
		if strings.Contains(b.Text, "PROTECTED-MUST-SURVIVE") {
			t.Errorf("protected block leaked into volatile UserBlocks: %q", b.Text)
		}
	}
	// Ordering preserved: same prefix text on the first block.
	if !strings.HasPrefix(got.UserBlocks[0].Text, "usr-0 ") {
		t.Errorf("block order changed: first block = %q", got.UserBlocks[0].Text)
	}
}

// ---------------------------------------------------------------------------
// D2 — budget-aware compression never returns nil and actually fits
// ---------------------------------------------------------------------------

func TestCTX51_D2_BudgetCompression(t *testing.T) {
	mk := func(n, chars int) []schemas.ContentBlock {
		out := make([]schemas.ContentBlock, n)
		for i := range out {
			out[i] = schemas.ContentBlock{Type: "text", Text: fmt.Sprintf("b%02d-%s", i, strings.Repeat("x", chars))}
		}
		return out
	}

	t.Run("fits_the_budget", func(t *testing.T) {
		blocks := mk(20, 5000)
		before := estimateBlocksTokens(blocks)
		res := compressUserBlocksToBudget(blocks, 20000)
		if res.Blocks == nil {
			t.Fatal("returned nil slice")
		}
		if !res.Changed {
			t.Fatalf("expected compression, reason=%q before=%d", res.Reason, before)
		}
		if res.AfterTok > 20000 {
			t.Errorf("still over budget: after=%d budget=20000", res.AfterTok)
		}
		if res.Blocks[0].Text != blocks[0].Text {
			t.Errorf("first block not preserved")
		}
		if res.Blocks[len(res.Blocks)-1].Text != blocks[len(blocks)-1].Text {
			t.Errorf("last block not preserved (truncation not expected at this budget)")
		}
	})

	t.Run("huge_budget_is_a_noop", func(t *testing.T) {
		res := compressUserBlocksToBudget(mk(20, 5000), 1<<30)
		if res.Changed {
			t.Errorf("compressed under a huge budget (reason=%q)", res.Reason)
		}
		if res.Reason != "under_budget" {
			t.Errorf("reason = %q, want under_budget", res.Reason)
		}
		if len(res.Blocks) != 20 {
			t.Errorf("blocks changed: %d", len(res.Blocks))
		}
	})

	t.Run("degenerate_inputs_never_nil", func(t *testing.T) {
		cases := map[string]struct {
			blocks []schemas.ContentBlock
			budget int
		}{
			"empty":        {nil, 100},
			"single_no_bud": {mk(1, 100), 0},
			"single_tight": {mk(1, 100000), 10},
			"smaller_than_keep": {mk(5, 100), 1},
		}
		for name, tc := range cases {
			res := compressUserBlocksToBudget(tc.blocks, tc.budget)
			if res.Blocks == nil {
				t.Errorf("%s: returned nil slice", name)
			}
			if res.Changed && res.Blocks == nil {
				t.Errorf("%s: changed but nil", name)
			}
		}
	})

	t.Run("truncates_one_fat_block", func(t *testing.T) {
		// 9 blocks: no middle to drop (2+6 == 8 < 9 drops exactly one), so the
		// budget can only be met by truncating the biggest block.
		blocks := mk(9, 1000)
		blocks[4] = schemas.ContentBlock{Type: "text", Text: strings.Repeat("F", 200000)}
		res := compressUserBlocksToBudget(blocks, 5000)
		if !res.Changed {
			t.Fatalf("expected change, reason=%q", res.Reason)
		}
		if res.AfterTok > 5000 {
			t.Errorf("still over budget after truncation: %d", res.AfterTok)
		}
		joined := ""
		for _, b := range res.Blocks {
			joined += b.Text
		}
		if !strings.Contains(joined, MarkerHistoryCompressed) {
			t.Errorf("no truncation marker in output")
		}
	})
}

// ---------------------------------------------------------------------------
// D4 — Audit must account for max_tokens (铁律 51-4)
// ---------------------------------------------------------------------------

func TestCTX51_D4_AuditCountsMaxTokens(t *testing.T) {
	// reserve is pinned to the tier default (0 would fall back to it anyway) so
	// the arithmetic is explicit: effectiveMaxCtx = 40960 - 4096 = 36864.
	const (
		win      = 40960
		reserve  = 4096
		maxOut   = 1871
		wantA    = 39704
	)

	newCM := func(stub *ctx51Stub) *ContextManager {
		reg := &config.Registry{
			System: config.SystemSettings{MaxContextKeep: 10},
			Providers: map[string]*config.ProviderConfig{
				"ctx51": {
					Provider:         stub.name,
					Model:            "m",
					PoolID:           "ctx51",
					MaxContextWindow: win,
					ReserveTokens:    reserve,
					// Pinned explicitly. Leaving this 0 used to be a SEPARATE
					// latent bug (effCtx stays 0, so compressibleBudget
					// collapses to 0 and every non-empty volatile prompt looks
					// "over budget" — the mechanical reason Squeeze ran every
					// turn on aw19). Fixed the next day in CTX52
					// (context_effective_window_test.go); the pin stays because
					// an explicitly configured soft ceiling is also a legitimate
					// configuration that must keep working.
					EffectiveContextWindow: win - reserve,
				},
			},
		}
		return NewContextManager(reg)
	}

	// build makes a request whose CountTokens sum is exactly total, split into
	// protected + volatile. countZone(volatile) adds a 2-byte "\n\n" separator.
	build := func(total, protected int) *schemas.CompleteRequest {
		volBlock := total - protected - 2
		return &schemas.CompleteRequest{
			ProtectedUserBlocks: []schemas.ContentBlock{{Type: "text", Text: strings.Repeat("P", protected)}},
			UserBlocks:          []schemas.ContentBlock{{Type: "text", Text: strings.Repeat("U", volBlock)}},
			Model:               "m",
			MaxTokens:           maxOut,
		}
	}

	t.Run("A_aw19_numbers_escalate", func(t *testing.T) {
		stub := &ctx51Stub{name: "ctx51-openai"}
		cm := newCM(stub)
		req := build(wantA, 20000)
		res, err := cm.Audit(context.Background(), stub, req)
		if err != nil {
			t.Fatalf("Audit: %v", err)
		}
		if res.OriginalTokens != wantA {
			t.Fatalf("fixture drift: OriginalTokens=%d want %d", res.OriginalTokens, wantA)
		}
		if res.Decision < LevelAggressive {
			t.Errorf("Decision=%v, want >= LevelAggressive", res.Decision)
		}
		if res.ActionTaken != "OUTPUT_BUDGET_PRESSURE" {
			t.Errorf("ActionTaken=%q, want OUTPUT_BUDGET_PRESSURE", res.ActionTaken)
		}
	})

	t.Run("B_headroom_is_not_flagged", func(t *testing.T) {
		stub := &ctx51Stub{name: "ctx51-openai"}
		cm := newCM(stub)
		req := build(30000, 29000)
		res, err := cm.Audit(context.Background(), stub, req)
		if err != nil {
			t.Fatalf("Audit: %v", err)
		}
		if res.Decision != LevelNone {
			t.Errorf("Decision=%v (%s), want LevelNone", res.Decision, res.ActionTaken)
		}
	})

	t.Run("C_never_forks_a_compressible_prompt", func(t *testing.T) {
		stub := &ctx51Stub{name: "ctx51-openai"}
		cm := newCM(stub)
		res, err := cm.Audit(context.Background(), stub, build(wantA, 20000))
		if err != nil {
			t.Fatalf("Audit: %v", err)
		}
		if res.Decision == LevelCritical {
			t.Errorf("forked a prompt that still fits the window (ActionTaken=%s)", res.ActionTaken)
		}
	})

	// Mutation guard (D4's own 反例): with MaxTokens dropped the A case must NOT
	// be flagged — that is the aw19 bug reproduced.
	t.Run("mutation_zero_max_tokens_does_not_fire", func(t *testing.T) {
		stub := &ctx51Stub{name: "ctx51-openai"}
		cm := newCM(stub)
		req := build(wantA, 20000)
		req.MaxTokens = 0
		res, err := cm.Audit(context.Background(), stub, req)
		if err != nil {
			t.Fatalf("Audit: %v", err)
		}
		if res.ActionTaken == "OUTPUT_BUDGET_PRESSURE" {
			t.Errorf("fired without max_tokens — the guard is not reading it")
		}
	})
}

// ---------------------------------------------------------------------------
// D3 — exactly one compression event per context-limit error (铁律 51-3)
// ---------------------------------------------------------------------------

// realLlamaContextLimit is the exact shape observed on aw19 (b3, turn 50).
var realLlamaContextLimit = errors.New(`bad request: {"error":{"code":400,"message":"request (41575 token: exce****","n_prompt_token: 4157****,"n_ctx":40960}}`)

func TestCTX51_D3_ExactlyOneCompressionEvent(t *testing.T) {
	// Phase A — roomy window. Nothing needs cutting, so the guard must take the
	// Skipped branch: the branch that used to leave NO trace at all. It also
	// measures the real prompt size T, which phase B needs to size its window.
	kindA, T := runCTX51EventCase(t, "roomy", 200000, 36000)
	if kindA != "skipped" {
		t.Fatalf("roomy window took the %q branch, want skipped", kindA)
	}
	if T <= 0 {
		t.Fatalf("stub never saw a prompt")
	}

	// Phase B — window sized from T so the prompt still fits the window but not
	// the reactive budget (budget = 0.85*(win-max_tokens) < T). Same task text,
	// so T carries over. This is the Compressed branch.
	win := T + 600
	kindB, T2 := runCTX51EventCase(t, "tight", win, 36000)
	if kindB != "compressed" {
		t.Fatalf("tight window (win=%d T=%d T2=%d) took the %q branch, want compressed",
			win, T, T2, kindB)
	}
}

// runCTX51EventCase drives one real doSpawn turn against a stub provider whose
// first call fails with the exact aw19 400, and reports which compression
// branch fired plus the prompt size the provider saw.
func runCTX51EventCase(t *testing.T, label string, window, taskLen int) (kind string, promptTokens int) {
	t.Helper()
	// Provider name is per-case so providers.Get's process-global cache can
	// never hand this case a stub left over from another case.
	stubName := fmt.Sprintf("ctx51-d3-%s-%d", label, window)
	// PoolID must be unique per case as well: GlobalRouter slots are looked up by
	// pool, so a shared pool id makes phase B silently reuse phase A's stub.
	poolID := fmt.Sprintf("ctx51d3-%s-%d", label, window)

	logDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	t.Cleanup(logger.Close)

	reg := &config.Registry{
		DefaultProvider: "ctx51d3",
		Providers: map[string]*config.ProviderConfig{
			"ctx51d3": {
				Provider:         stubName,
				Model:            "m",
				PoolID:           poolID,
				MaxContextWindow: window,
				ReserveTokens:    256,
				// Pinned so Audit's tier math is not driven by a zero (see
				// D4's note about EffectiveContextWindow).
				EffectiveContextWindow: window - 256,
				APIKeyEnv:              "CTX51_TEST_KEY",
			},
		},
		Roles: map[string]*config.Role{
			"r1": {ID: "r1", Provider: "ctx51d3"},
		},
		System: config.SystemSettings{MaxContextKeep: 10},
	}

	stub := &ctx51Stub{name: stubName, failFirst: 1, failErr: realLlamaContextLimit, perToken: 4}

	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)
	s := NewSpawner(reg, ts, nil, logger, &ResourceLoader{}, "outputs")

	taskID, stepID := "task_ctx51", "s1"
	hub := NewContextHub(reg, nil, nil)

	// Inject via the documented test hook: providers.Get consults GlobalRouter
	// before building a real client. The cache key is the one providers.Get will
	// compute, so derive it from the exact config the spawner will resolve —
	// guessing it from the raw registry entry misses api-key resolution.
	pc := hub.GetProviderConfig("ctx51d3")
	if pc == nil {
		t.Fatal("fixture: provider config did not resolve")
	}
	key := fmt.Sprintf("%s::%s::%s::%s", pc.Provider, pc.BaseURL, pc.APIKeyEnv, pc.APIKey)
	providers.GlobalRouter.Register(key, pc, stub)
	t.Cleanup(func() { providers.GlobalRouter.Deregister(key) })

	req := &SpawnRequest{TaskID: taskID, StepID: stepID, RoleID: "r1", Task: "do the thing " + strings.Repeat("x", taskLen), Hub: hub}

	_, spawnErr := s.doSpawn(context.Background(), req)
	logger.Close()

	date := time.Now().UTC().Format("2006-01-02")
	raw, err := os.ReadFile(filepath.Join(logDir, date, taskID+".jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logStr := string(raw)

	compressed := strings.Count(logStr, string(EventContextCompressed))
	skipped := strings.Count(logStr, `"`+string(EventContextCompressSkipped)+`"`)
	total := compressed + skipped

	calls, _, _ := stub.stats()
	promptTokens = stub.promptTokens
	if calls == 0 {
		mx := s.spawnOutputBudget(req)
		ar, aerr := s.contextManager.Audit(context.Background(), stub, &schemas.CompleteRequest{
			SystemBlocks: stub.lastSystem, UserBlocks: stub.lastUser, Model: "m", MaxTokens: mx,
		})
		t.Logf("DIAG maxTokens=%d audit=%v/%s total=%d schema=%d err=%v tok=%d", mx, ar.Decision, ar.ActionTaken, ar.OriginalTokens, ar.MCSchemaTokens, aerr, promptTokens)
		t.Fatalf("%s: provider was never called (win=%d spawnErr=%v) — the fixture did not reach the guard\nlog:\n%s",
			label, window, spawnErr, logStr)
	}
	if total != 1 {
		t.Fatalf("%s: compression events = %d (compressed=%d skipped=%d), want exactly 1\nlog:\n%s",
			label, total, compressed, skipped, logStr)
	}
	if compressed == 1 {
		return "compressed", promptTokens
	}
	legal := []string{"under_budget", "no_middle", "no_budget", "empty", "truncated_only"}
	for _, r := range legal {
		if strings.Contains(logStr, `"reason":"`+r+`"`) {
			return "skipped", promptTokens
		}
	}
	t.Errorf("%s: skipped event carries a reason outside the declared enum:\n%s", label, logStr)
	return "unknown", promptTokens
}
