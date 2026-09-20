package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// newAntiCliffRegistry builds a fixed-limit provider config for deterministic
// tier math. Tokens are counted by len(text)/4 (via MockProvider.CountTokens).
func newAntiCliffRegistry(maxCtx, effCtx, tokenLimit int) *config.Registry {
	reg := &config.Registry{Providers: make(map[string]*config.ProviderConfig)}
	reg.Providers["anti"] = &config.ProviderConfig{
		Provider:               "anti",
		MaxContextWindow:       maxCtx,
		EffectiveContextWindow: effCtx,
		TokenLimit:             tokenLimit,
	}
	return reg
}

// tokenText builds a string that MockProvider.CountTokens will count as exactly
// n tokens. n*4 chars, all lowercase letters (no spaces so RTK won't strip).
func tokenText(n int) string {
	return strings.Repeat("a", n*4)
}

// tokenBlock builds a ContentBlock that MockProvider.CountTokens counts as n tokens.
func tokenBlock(n int) schemas.ContentBlock {
	return schemas.ContentBlock{Text: tokenText(n)}
}

// ---------- Audit tests ----------

// TestAntiCliff_Audit_VolatileFitsInRemainingBudget verifies the core
// Anti-Cliff claim: when Protected occupies X tokens and Volatile fits in
// (effCtx - X) compressible headroom, Audit returns LevelNone even if the
// legacy flat view would have flagged the total.
//
// Layout: maxCtx=200, effCtx=200, Protected=20 tok, Volatile=30 tok.
// Remaining budget = 200-20 = 180 ≥ 30 → LevelNone (PASS).
func TestAntiCliff_Audit_VolatileFitsInRemainingBudget(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(200, 200, 200))
	p := &MockProvider{name: "anti"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(20)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(30)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelNone {
		t.Errorf("expected LevelNone (volatile 30 fits in remaining 180), got %v (%s)", res.Decision, res.ActionTaken)
	}
}

// TestAntiCliff_Audit_VolatileOverBudgetTriggersCompression verifies that
// when the Volatile Zone alone exceeds its remaining compressible budget,
// Audit correctly escalates — but crucially, only the Volatile Zone will be
// compressed by Squeeze (tested in TestAntiCliff_Squeeze_VolatileOnly).
//
// Layout: maxCtx=500, effCtx=100, Protected=20 tok, Volatile=90 tok.
// Compressible budget = effCtx(100) - Protected(20) = 80.
// Volatile(90) > 80 → LevelAggressive (and total=110 < maxCtx=500, so no Fork).
func TestAntiCliff_Audit_VolatileOverBudgetTriggersCompression(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(500, 100, 500))
	p := &MockProvider{name: "anti"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(20)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(90)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelAggressive {
		t.Errorf("expected LevelAggressive (volatile 90 > remaining 80), got %v (%s)", res.Decision, res.ActionTaken)
	}
}

// TestAntiCliff_Audit_ProtectedOverrun75_ForcesFork verifies the
// uncompressible-floor guarantee: when Protected alone exceeds 75% of
// maxCtx, no amount of volatile compression can help — Audit returns
// LevelCritical with PROTECTED_ZONE_OVERRUN_FORK.
//
// Layout: maxCtx=200 → 75% = 150. Protected=160 → must Fork.
func TestAntiCliff_Audit_ProtectedOverrun75_ForcesFork(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(200, 200, 200))
	p := &MockProvider{name: "anti"}

	// Protected = 160 tok (> 75%% of 200 = 150)
	// Volatile tiny so total stays under maxCtx
	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(160)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(5)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelCritical {
		t.Errorf("expected LevelCritical when protected zone overruns, got %v (%s)", res.Decision, res.ActionTaken)
	}
	if res.ActionTaken != "PROTECTED_ZONE_OVERRUN_FORK" {
		t.Errorf("expected PROTECTED_ZONE_OVERRUN_FORK, got %s", res.ActionTaken)
	}
}

// TestAntiCliff_Audit_ProtectedAtExactly75_DoesNotFork verifies the
// threshold boundary: 75%% exactly does NOT trigger Fork (strict >).
func TestAntiCliff_Audit_ProtectedAtExactly75_DoesNotFork(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(200, 200, 200))
	p := &MockProvider{name: "anti"}

	// Protected = 150 tok (= 75%% of 200)
	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(150)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(10)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken == "PROTECTED_ZONE_OVERRUN_FORK" {
		t.Error("75%% exactly should not trigger PROTECTED_ZONE_OVERRUN_FORK")
	}
}

// ---------- Squeeze tests ----------

// TestAntiCliff_Squeeze_ProtectedVerbatimAtAllLevels verifies the mechanical
// guarantee: ProtectedSystemBlocks / ProtectedUserBlocks flow through
// Squeeze untouched at LevelLight AND LevelAggressive, while legacy
// UserBlocks (Volatile) DOES get compressed. The differential proves the
// two zones are independently processed.
func TestAntiCliff_Squeeze_ProtectedVerbatimAtAllLevels(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(10000, 5000, 10000))
	p := &MockProvider{name: "anti"}

	protectedText := "SOP RULE 42: never delete files without user confirmation"
	volatileText := "Processing...[2026-08-27 12:00:00] Progress: 10%[2026-08-27 12:00:01] Updating..."

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{{Text: protectedText}},
		// Use legacy UserBlocks (Volatile) — these get RTK-filtered through
		// the string-based code path in Squeeze.
		UserBlocks: []schemas.ContentBlock{{Text: volatileText}},
	}

	for _, lvl := range []CompressionLevel{LevelLight, LevelAggressive} {
		t.Run(fmt.Sprintf("level=%d", lvl), func(t *testing.T) {
			squeezed, _, err := cm.Squeeze(context.Background(), p, req, lvl)
			if err != nil {
				t.Fatalf("Squeeze failed at %d: %v", lvl, err)
			}
			// Protected verbatim
			if len(squeezed.ProtectedSystemBlocks) != 1 {
				t.Fatalf("ProtectedSystemBlocks length changed: got %d", len(squeezed.ProtectedSystemBlocks))
			}
			if squeezed.ProtectedSystemBlocks[0].Text != protectedText {
				t.Errorf("ProtectedSystemBlocks mutated at level %d: got %q", lvl, squeezed.ProtectedSystemBlocks[0].Text)
			}
			// Volatile (legacy UserBlocks) was RTK-filtered
			if squeezed.User == volatileText {
				t.Errorf("Volatile legacy UserBlocks unchanged at level %d (RTK should have filtered)", lvl)
			}
			// Flat System must still contain protected text at top
			if !strings.Contains(squeezed.System, protectedText) {
				t.Error("System missing protected text — legacy provider path would lose rules")
			}
		})
	}
}

// TestAntiCliff_Squeeze_ProtectedMergedIntoSystem verifies that when
// consumers only inspect the flat System string (legacy provider path),
// the protected content is still present at the top of System — Squeeze
// prepends protected verbatim before the compressed volatile payload.
func TestAntiCliff_Squeeze_ProtectedMergedIntoSystem(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(10000, 5000, 10000))
	p := &MockProvider{name: "anti"}

	protectedText := "SOP RULE 42: never delete files without user confirmation"
	volatileText := "Processing...[2026-08-27 12:00:00] Progress: 50%[2026-08-27 12:00:01] Updating..."

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{{Text: protectedText}},
		VolatileUserBlocks:    []schemas.ContentBlock{{Text: volatileText}},
	}
	squeezed, _, err := cm.Squeeze(context.Background(), p, req, LevelLight)
	if err != nil {
		t.Fatalf("Squeeze failed: %v", err)
	}
	if !strings.Contains(squeezed.System, protectedText) {
		t.Error("flat System string missing protected text — legacy provider path would lose rules")
	}
}

// TestAntiCliff_Squeeze_LegacyFieldsStillCompressible verifies backward
// compatibility: legacy System/User string fields are treated as Volatile
// Zone and remain compressible by Squeeze. Pre-Anti-Cliff code paths that
// haven't migrated to the Protected*/Volatile* block API continue to work.
func TestAntiCliff_Squeeze_LegacyFieldsStillCompressible(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(10000, 5000, 10000))
	p := &MockProvider{name: "anti"}

	volatile := "Processing...[2026-08-27 12:00:00] Progress: 50%[2026-08-27 12:00:01] Updating..."
	req := &schemas.CompleteRequest{
		UserBlocks: []schemas.ContentBlock{{Text: volatile}},
	}
	squeezed, _, err := cm.Squeeze(context.Background(), p, req, LevelLight)
	if err != nil {
		t.Fatalf("Squeeze failed: %v", err)
	}
	if squeezed.User == volatile {
		t.Error("legacy UserBlocks should be RTK-filtered, but User was unchanged")
	}
}

// TestAntiCliff_Audit_LegacyFieldsAsVolatile verifies that legacy System/
// User strings are classified as Volatile (compressible) Zone. This is
// the "no-surprise migration" property: old code continues to get
// compressed the same way; the anti-cliff protection only activates when
// consumers explicitly use Protected*/Volatile* blocks.
func TestAntiCliff_Audit_LegacyFieldsAsVolatile(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(200, 200, 200))
	p := &MockProvider{name: "anti"}

	req := &schemas.CompleteRequest{
		System: tokenText(10),
		User:   tokenText(10),
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	// Both zones tiny → LevelNone
	if res.Decision != LevelNone {
		t.Errorf("legacy short fields should pass LevelNone, got %v", res.Decision)
	}

	// Now blow up User to exceed compressible budget (all treated as Volatile)
	req.User = tokenText(210)
	res, err = cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	// total=220 < maxCtx=200? No, so we need maxCtx > 220 to avoid Fork.
	// Rebuild registry with maxCtx=400, effCtx=200 so total fits but volatile
	// still exceeds remaining budget.
	cm = NewContextManager(newAntiCliffRegistry(400, 200, 400))
	res, err = cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelAggressive {
		t.Errorf("expected LevelAggressive when legacy volatile exceeds effCtx, got %v (%s)", res.Decision, res.ActionTaken)
	}
}

// TestAntiCliff_Squeeze_ProtectedUserBlocksPreserved verifies the user-side
// Protected Zone (ProtectedUserBlocks) also flows through verbatim.
func TestAntiCliff_Squeeze_ProtectedUserBlocksPreserved(t *testing.T) {
	cm := NewContextManager(newAntiCliffRegistry(10000, 5000, 10000))
	p := &MockProvider{name: "anti"}

	protectedUser := "GOAL: answer must include field 'count' and 'risk_level'"
	volatileUser := "Progress: 10%[2026-08-27 12:00:00] Processing..."

	req := &schemas.CompleteRequest{
		ProtectedUserBlocks: []schemas.ContentBlock{{Text: protectedUser}},
		VolatileUserBlocks:  []schemas.ContentBlock{{Text: volatileUser}},
	}
	squeezed, _, err := cm.Squeeze(context.Background(), p, req, LevelLight)
	if err != nil {
		t.Fatalf("Squeeze failed: %v", err)
	}
	if len(squeezed.ProtectedUserBlocks) != 1 {
		t.Fatalf("ProtectedUserBlocks length: got %d", len(squeezed.ProtectedUserBlocks))
	}
	if squeezed.ProtectedUserBlocks[0].Text != protectedUser {
		t.Errorf("ProtectedUserBlocks mutated: got %q, want %q", squeezed.ProtectedUserBlocks[0].Text, protectedUser)
	}
}
