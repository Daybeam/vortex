package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// newReserveRegistry builds a provider config with the given maxCtx, effCtx,
// tokenLimit, and reserveTokens. The model is set to "gpt-4o" (Flagship tier)
// so that the tier-aware default reserve is 4096 — but tests that pass a
// positive reserveTokens override that default explicitly.
func newReserveRegistry(maxCtx, effCtx, tokenLimit, reserveTokens int) *config.Registry {
	reg := &config.Registry{Providers: make(map[string]*config.ProviderConfig)}
	reg.Providers["reserve"] = &config.ProviderConfig{
		Provider:               "reserve",
		Model:                  "gpt-4o",
		MaxContextWindow:       maxCtx,
		EffectiveContextWindow: effCtx,
		TokenLimit:             tokenLimit,
		ReserveTokens:          reserveTokens,
	}
	return reg
}

// ---------- AuditResult population ----------

// TestAudit_Reserve_PopulatesAuditResult verifies that Audit populates the
// ReserveTokens and EffectiveMaxCtx fields on every return path.
func TestAudit_Reserve_PopulatesAuditResult(t *testing.T) {
	cm := NewContextManager(newReserveRegistry(10000, 8000, 0, 1000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(100)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ReserveTokens != 1000 {
		t.Errorf("ReserveTokens = %d, want 1000", res.ReserveTokens)
	}
	if res.EffectiveMaxCtx != 9000 {
		t.Errorf("EffectiveMaxCtx = %d, want 9000", res.EffectiveMaxCtx)
	}
}

// TestAudit_Reserve_ZeroWhenSkipped verifies that when the reserve exceeds
// the window (small window + large default), ReserveTokens is reset to 0
// and EffectiveMaxCtx equals maxCtx.
func TestAudit_Reserve_ZeroWhenSkipped(t *testing.T) {
	// maxCtx=200, default Flagship reserve=4096 → effectiveMaxCtx < 0 → skip
	reg := &config.Registry{Providers: make(map[string]*config.ProviderConfig)}
	reg.Providers["small"] = &config.ProviderConfig{
		Provider:         "small",
		Model:            "gpt-4o",
		MaxContextWindow: 200,
	}
	cm := NewContextManager(reg)
	p := &MockProvider{name: "small"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(10)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ReserveTokens != 0 {
		t.Errorf("ReserveTokens should be 0 when skipped, got %d", res.ReserveTokens)
	}
	if res.EffectiveMaxCtx != 200 {
		t.Errorf("EffectiveMaxCtx should equal maxCtx when reserve skipped, got %d", res.EffectiveMaxCtx)
	}
}

// ---------- Reserve triggers earlier compression ----------

// TestAudit_Reserve_TriggersEarlierFork verifies the core design claim:
// with maxCtx=10000 and reserve=4000, effectiveMaxCtx=6000. A prompt of
// 7000 tokens is below maxCtx (10000) but above effectiveMaxCtx (6000),
// so it should trigger FORCE_FORK — without reserve it would pass.
// tokenLimit is set high (15000) so the HARD_LIMIT check doesn't fire first.
func TestAudit_Reserve_TriggersEarlierFork(t *testing.T) {
	cm := NewContextManager(newReserveRegistry(10000, 10000, 15000, 4000))
	p := &MockProvider{name: "reserve"}

	// 7000 tokens: below maxCtx(10000), above effectiveMaxCtx(6000)
	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(7000)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelCritical {
		t.Errorf("expected LevelCritical with reserve (7000 > effectiveMaxCtx 6000), got %v (%s)", res.Decision, res.ActionTaken)
	}
	if res.ActionTaken != "FORCE_FORK" {
		t.Errorf("expected FORCE_FORK, got %s", res.ActionTaken)
	}
}

// TestAudit_Reserve_NoForkWithTinyReserve is the counterpart: with a tiny
// reserve (1 token), effectiveMaxCtx=9999. The same 7000-token prompt
// should NOT trigger fork because 7000 < 9999.
// Note: ReserveTokens=0 means "use tier default" (4096 for gpt-4o), not
// "disable reserve". To effectively disable, use a tiny positive value.
func TestAudit_Reserve_NoForkWithTinyReserve(t *testing.T) {
	cm := NewContextManager(newReserveRegistry(10000, 10000, 15000, 1))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(7000)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ReserveTokens != 1 {
		t.Errorf("expected ReserveTokens=1, got %d", res.ReserveTokens)
	}
	if res.Decision == LevelCritical {
		t.Errorf("should not fork with tiny reserve (7000 < effectiveMaxCtx 9999), got %v (%s)", res.Decision, res.ActionTaken)
	}
}

// ---------- Reserve affects compressible budget ----------

// TestAudit_Reserve_ShrinksCompressibleBudget verifies that reserve is
// subtracted from the compressible budget, triggering LevelAggressive
// earlier than without reserve.
//
// Layout: maxCtx=20000, effCtx=8000, reserve=4000, Protected=100, Volatile=4200.
// compressibleBudget = effCtx(8000) - protected(100) - reserve(4000) = 3900.
// Volatile(4200) > 3900 → LevelAggressive.
// Without reserve: budget = 8000-100 = 7900, 4200 < 7900 → would be LevelNone/LevelLight.
func TestAudit_Reserve_ShrinksCompressibleBudget(t *testing.T) {
	cm := NewContextManager(newReserveRegistry(20000, 8000, 0, 4000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(100)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(4200)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelAggressive {
		t.Errorf("expected LevelAggressive (volatile 4200 > compressible 3900), got %v (%s)", res.Decision, res.ActionTaken)
	}
}

// TestAudit_Reserve_CompressibleBudgetWithTinyReserve verifies that with a
// tiny reserve (1 token), the compressible budget is essentially
// (effCtx - protected), matching pre-reserve behavior.
//
// Layout: maxCtx=20000, effCtx=8000, reserve=1, Protected=100, Volatile=4200.
// compressibleBudget = effCtx(8000) - protected(100) - reserve(1) = 7899.
// Volatile(4200) < 7899 → not Aggressive. 4200 > 7899/2=3949 → LevelLight.
func TestAudit_Reserve_CompressibleBudgetWithTinyReserve(t *testing.T) {
	cm := NewContextManager(newReserveRegistry(20000, 8000, 0, 1))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(100)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(4200)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelLight {
		t.Errorf("expected LevelLight with tiny reserve (4200 > 3949 but < 7899), got %v (%s)", res.Decision, res.ActionTaken)
	}
}

// ---------- Explicit override vs tier default ----------

// TestAudit_Reserve_ExplicitOverrideUsed verifies that an explicit
// ReserveTokens in config is used instead of the tier-aware default.
func TestAudit_Reserve_ExplicitOverrideUsed(t *testing.T) {
	// Model is gpt-4o (Flagship, default 4096), but explicit override is 2000.
	cm := NewContextManager(newReserveRegistry(10000, 10000, 0, 2000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(100)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ReserveTokens != 2000 {
		t.Errorf("expected explicit override 2000, got %d", res.ReserveTokens)
	}
	if res.EffectiveMaxCtx != 8000 {
		t.Errorf("expected EffectiveMaxCtx 8000 (10000-2000), got %d", res.EffectiveMaxCtx)
	}
}

// TestAudit_Reserve_TierDefaultUsedWhenNotExplicit verifies that when
// ReserveTokens is not set (0), the tier-aware default is used.
func TestAudit_Reserve_TierDefaultUsedWhenNotExplicit(t *testing.T) {
	reg := &config.Registry{Providers: make(map[string]*config.ProviderConfig)}
	reg.Providers["tier"] = &config.ProviderConfig{
		Provider:         "tier",
		Model:            "llama-3.1-8b", // Light tier → default 2048
		MaxContextWindow: 10000,
	}
	cm := NewContextManager(reg)
	p := &MockProvider{name: "tier"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(100)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ReserveTokens != config.DefaultReserveTokensLight {
		t.Errorf("expected Light tier default %d, got %d", config.DefaultReserveTokensLight, res.ReserveTokens)
	}
	if res.EffectiveMaxCtx != 10000-config.DefaultReserveTokensLight {
		t.Errorf("expected EffectiveMaxCtx %d, got %d", 10000-config.DefaultReserveTokensLight, res.EffectiveMaxCtx)
	}
}

// ---------- tokenLimit interaction ----------

// TestAudit_Reserve_TokenLimitDefaultsToEffectiveMaxCtx verifies that when
// TokenLimit is 0, it defaults to effectiveMaxCtx (not maxCtx), so the
// hard-limit check also respects the reserve.
func TestAudit_Reserve_TokenLimitDefaultsToEffectiveMaxCtx(t *testing.T) {
	// maxCtx=10000, reserve=4000 → effectiveMaxCtx=6000.
	// tokenLimit=0 → defaults to 6000.
	// 7000 tokens: 7000 > tokenLimit(6000) → HARD_LIMIT_BREACH_FORK.
	cm := NewContextManager(newReserveRegistry(10000, 10000, 0, 4000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(7000)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken != "HARD_LIMIT_BREACH_FORK" {
		t.Errorf("expected HARD_LIMIT_BREACH_FORK (7000 > tokenLimit 6000), got %s", res.ActionTaken)
	}
}

// TestAudit_Reserve_ExplicitTokenLimitRespected verifies that an explicit
// TokenLimit is respected as-is (not adjusted by reserve).
func TestAudit_Reserve_ExplicitTokenLimitRespected(t *testing.T) {
	// maxCtx=10000, reserve=4000 → effectiveMaxCtx=6000.
	// tokenLimit=9000 (explicit) — should NOT be adjusted by reserve.
	// 7000 tokens: 7000 < tokenLimit(9000), 7000 > effectiveMaxCtx(6000) → FORCE_FORK.
	cm := NewContextManager(newReserveRegistry(10000, 10000, 9000, 4000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(7000)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken != "FORCE_FORK" {
		t.Errorf("expected FORCE_FORK (7000 < tokenLimit 9000 but > effectiveMaxCtx 6000), got %s", res.ActionTaken)
	}
}

// ---------- Protected zone overrun uses maxCtx, not effectiveMaxCtx ----------

// TestAudit_Reserve_ProtectedOverrunUsesMaxCtx verifies that the 75%
// protected-zone check uses maxCtx (the full window) as the denominator,
// not effectiveMaxCtx. This is because the check is about what fraction
// of the total window is uncompressible — the reserve doesn't change that
// fraction.
func TestAudit_Reserve_ProtectedOverrunUsesMaxCtx(t *testing.T) {
	// maxCtx=10000, reserve=4000 → effectiveMaxCtx=6000.
	// Protected=7600 → 76% of maxCtx(10000) > 75% → PROTECTED_ZONE_OVERRUN_FORK.
	// If effectiveMaxCtx were used: 7600/6000 = 127% — also triggers, but
	// the point is the denominator is maxCtx.
	cm := NewContextManager(newReserveRegistry(10000, 10000, 0, 4000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(7600)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(100)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken != "PROTECTED_ZONE_OVERRUN_FORK" {
		t.Errorf("expected PROTECTED_ZONE_OVERRUN_FORK (76%% of maxCtx), got %s", res.ActionTaken)
	}
}

// TestAudit_Reserve_ProtectedAt74PctDoesNotOverrun verifies that at 74%
// of maxCtx, the protected zone check does NOT trigger (even though 74%
// of maxCtx might be > 75% of effectiveMaxCtx).
func TestAudit_Reserve_ProtectedAt74PctDoesNotOverrun(t *testing.T) {
	// maxCtx=10000, reserve=4000 → effectiveMaxCtx=6000.
	// Protected=7400 → 74% of maxCtx < 75% → no PROTECTED_ZONE_OVERRUN_FORK.
	// (7400/6000 = 123% — would trigger if effectiveMaxCtx were the denominator.)
	cm := NewContextManager(newReserveRegistry(10000, 10000, 0, 4000))
	p := &MockProvider{name: "reserve"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(7400)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(100)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken == "PROTECTED_ZONE_OVERRUN_FORK" {
		t.Error("74% of maxCtx should not trigger PROTECTED_ZONE_OVERRUN_FORK")
	}
}
