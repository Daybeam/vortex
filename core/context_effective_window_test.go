package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// PREREG-CTX52: EffectiveContextWindow has no fallback, so effCtx stays 0 in
// every deployment that does not set it by hand (the AppWorld harness does not:
// it only writes max_context_window). compressibleBudget then clamps to 0 and
// any non-empty volatile prompt is judged LevelAggressive — every turn.
//
// These fixtures pin the provider-visible token counts exactly (perToken=0 =>
// CountTokens returns len(s)), same convention as the CTX51 suite.

// ctx52CM builds a ContextManager whose only provider is stub.name.
func ctx52CM(stub *ctx51Stub, mutate func(*config.ProviderConfig)) *ContextManager {
	pc := &config.ProviderConfig{
		Provider: stub.name,
		Model:    "m",
		PoolID:   "ctx52",
	}
	if mutate != nil {
		mutate(pc)
	}
	reg := &config.Registry{
		System:    config.SystemSettings{MaxContextKeep: 10},
		Providers: map[string]*config.ProviderConfig{pc.PoolID: pc},
	}
	return NewContextManager(reg)
}

// ctx52Req builds a request whose counts sum to exactly total (protected +
// volatile), accounting for countZone's 2-byte "\n\n" join on the volatile side.
func ctx52Req(total, protected int) *schemas.CompleteRequest {
	volText := total - protected - 2
	if volText < 0 {
		volText = 0
	}
	return &schemas.CompleteRequest{
		ProtectedUserBlocks: []schemas.ContentBlock{{Type: "text", Text: strings.Repeat("P", protected)}},
		UserBlocks:          []schemas.ContentBlock{{Type: "text", Text: strings.Repeat("U", volText)}},
		Model:               "m",
	}
}

// E1 — the regression itself. Before the fix this returns LevelAggressive
// ("SEMANTIC_PRUNING") purely because effCtx is zero; the window is 73% empty.
func TestCTX52_E1_UnsetEffectiveWindowIsNotAggressive(t *testing.T) {
	stub := &ctx51Stub{name: "ctx52-openai"}
	cm := ctx52CM(stub, func(pc *config.ProviderConfig) {
		pc.MaxContextWindow = 40960
		pc.ReserveTokens = 4096
		// EffectiveContextWindow deliberately unset (0) — this is what
		// kernel.py and harness/config_appworld_kaggle.json produce.
		// MaxTokens deliberately unset so 铁律 51-4 (OUTPUT_BUDGET_PRESSURE)
		// cannot fire first and mask the bug under test.
	})

	const (
		total     = 30000
		protected = 29000
	)
	req := ctx52Req(total, protected)

	res, err := cm.Audit(context.Background(), stub, req)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if res.OriginalTokens != total {
		t.Fatalf("fixture drift: OriginalTokens=%d want %d", res.OriginalTokens, total)
	}
	// effectiveMaxCtx = 36864, compressibleBudget = 36864 - 29000 - 4096 = 3768,
	// volatile 1000 < 3768/2 -> nothing to do.
	if res.Decision != LevelNone {
		t.Errorf("Decision=%v (%s), want LevelNone: window is %.0f%% empty, "+
			"effCtx must fall back to effectiveMaxCtx (%d)",
			res.Decision, res.ActionTaken, 100*float64(total)/40960, 36864)
	}
}

// E2 — the guard against over-fixing: an EXPLICIT EffectiveContextWindow must
// still be honoured even when it is much tighter than maxCtx. If the fix is
// "always use effectiveMaxCtx", this case goes LevelNone and fails.
func TestCTX52_E2_ExplicitEffectiveWindowStillHonoured(t *testing.T) {
	stub := &ctx51Stub{name: "ctx52-openai"}
	cm := ctx52CM(stub, func(pc *config.ProviderConfig) {
		pc.MaxContextWindow = 128000
		pc.EffectiveContextWindow = 32000 // soft ceiling, as config_windows.json does
		pc.ReserveTokens = 4096
	})

	const (
		total     = 30000
		protected = 20000
	)
	req := ctx52Req(total, protected)

	res, err := cm.Audit(context.Background(), stub, req)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	// budget vs the soft ceiling: 32000 - 20000 - 4096 = 7904 < volatile 10000.
	// budget vs the hard window:  123904 - 20000 - 4096 = 99808 -> would be None.
	if res.Decision != LevelAggressive {
		t.Errorf("Decision=%v (%s), want LevelAggressive: effective_context_window=32000 "+
			"is an explicit soft ceiling and must not be replaced by maxCtx-reserve",
			res.Decision, res.ActionTaken)
	}
}

// E3/E3b — the fallback must be the thing hints scale from. Paired on purpose:
// with effCtx=0 both cases are Aggressive (0/2 == 0), so only a working
// fallback makes the pair split into None/Aggressive.
func TestCTX52_E3_HintScalesFromFallenBackWindow(t *testing.T) {
	const (
		total     = 550
		protected = 200
	)

	audit := func(t *testing.T, hint string) *AuditResult {
		t.Helper()
		stub := &ctx51Stub{name: "ctx52-openai"}
		cm := ctx52CM(stub, func(pc *config.ProviderConfig) {
			pc.MaxContextWindow = 1000
			// ReserveTokens unset: 1000 - 4096 <= 0 -> Audit drops the reserve
			// and uses maxCtx, so effectiveMaxCtx = 1000 exactly.
			// EffectiveContextWindow unset again.
		})
		req := ctx52Req(total, protected)
		req.CompressionHint = hint
		res, err := cm.Audit(context.Background(), stub, req)
		if err != nil {
			t.Fatalf("Audit(%q): %v", hint, err)
		}
		return res
	}

	// No hint: budget 1000-200 = 800, volatile 350 < 400 -> None.
	if res := audit(t, ""); res.Decision != LevelNone {
		t.Errorf("no hint: Decision=%v (%s), want LevelNone (budget 800, volatile 350)",
			res.Decision, res.ActionTaken)
	}
	// terse halves the *real* capacity: 500-200 = 300 < volatile 350 -> Aggressive.
	if res := audit(t, "terse"); res.Decision != LevelAggressive {
		t.Errorf("terse: Decision=%v (%s), want LevelAggressive (halved budget 300, volatile 350)",
			res.Decision, res.ActionTaken)
	}
}
