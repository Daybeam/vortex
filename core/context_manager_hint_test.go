package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestApplyCompressionHint covers the pure threshold-scaling function
// directly, independent of the Audit() integration.
func TestApplyCompressionHint(t *testing.T) {
	cases := []struct {
		name           string
		effCtx, maxCtx int
		hint           string
		want           int
	}{
		{"terse halves", 400, 1000, "terse", 200},
		{"aggressive halves", 400, 1000, "aggressive", 200},
		{"verbose raises by 50pct", 400, 1000, "verbose", 600},
		{"verbose capped at maxCtx", 800, 1000, "verbose", 1000},
		{"empty hint unchanged", 400, 1000, "", 400},
		{"unrecognized hint unchanged", 400, 1000, "whatever", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyCompressionHint(c.effCtx, c.maxCtx, c.hint)
			if got != c.want {
				t.Fatalf("applyCompressionHint(%d, %d, %q) = %d, want %d", c.effCtx, c.maxCtx, c.hint, got, c.want)
			}
		})
	}
}

// newHintTestRegistry builds a fixed-limit provider config for deterministic
// threshold math: MaxContextWindow=1000, EffectiveContextWindow=400,
// TokenLimit=1200. Combined with MockProvider.CountTokens (len(text)/4),
// this makes the exact token count of a fixture string directly control
// which Decision tier Audit lands on.
func newHintTestRegistry() *config.Registry {
	reg := &config.Registry{Providers: make(map[string]*config.ProviderConfig)}
	reg.Providers["gemini"] = &config.ProviderConfig{
		Provider:               "gemini",
		MaxContextWindow:       1000,
		EffectiveContextWindow: 400,
		TokenLimit:             1200,
	}
	return reg
}

// textOfTokens returns a string that MockProvider.CountTokens will count as
// exactly n tokens (n*4 characters).
func textOfTokens(n int) string {
	return strings.Repeat("x", n*4)
}

// TestAudit_CompressionHintTerse_TriggersEarlierCompression is a direct
// regression test showing the hint changes Audit”s real decision, not just
// the pure helper function in isolation. 150 tokens sits below the default
// effCtx/2 (200) threshold -- LevelNone without a hint -- but above the
// terse-halved threshold (100), so it should trigger LevelLight once a
// step opts in.
func TestAudit_CompressionHintTerse_TriggersEarlierCompression(t *testing.T) {
	cm := NewContextManager(newHintTestRegistry())
	provider := &MockProvider{name: "gemini"}
	text := textOfTokens(150)

	baseline, err := cm.Audit(context.Background(), provider, &schemas.CompleteRequest{System: text})
	if err != nil {
		t.Fatalf("baseline Audit failed: %v", err)
	}
	if baseline.Decision != LevelNone {
		t.Fatalf("expected LevelNone without a hint at 150 tokens, got %v", baseline.Decision)
	}

	hinted, err := cm.Audit(context.Background(), provider, &schemas.CompleteRequest{System: text, CompressionHint: "terse"})
	if err != nil {
		t.Fatalf("hinted Audit failed: %v", err)
	}
	if hinted.Decision != LevelLight {
		t.Fatalf("expected LevelLight with terse hint at 150 tokens, got %v", hinted.Decision)
	}
}

// TestAudit_CompressionHintVerbose_DelaysCompression is the inverse: 250
// tokens triggers LevelLight by default (above effCtx/2=200) but should
// stay LevelNone once a step opts into "verbose" (raises the soft
// threshold to 300).
func TestAudit_CompressionHintVerbose_DelaysCompression(t *testing.T) {
	cm := NewContextManager(newHintTestRegistry())
	provider := &MockProvider{name: "gemini"}
	text := textOfTokens(250)

	baseline, err := cm.Audit(context.Background(), provider, &schemas.CompleteRequest{System: text})
	if err != nil {
		t.Fatalf("baseline Audit failed: %v", err)
	}
	if baseline.Decision != LevelLight {
		t.Fatalf("expected LevelLight without a hint at 250 tokens, got %v", baseline.Decision)
	}

	hinted, err := cm.Audit(context.Background(), provider, &schemas.CompleteRequest{System: text, CompressionHint: "verbose"})
	if err != nil {
		t.Fatalf("hinted Audit failed: %v", err)
	}
	if hinted.Decision != LevelNone {
		t.Fatalf("expected LevelNone with verbose hint at 250 tokens, got %v", hinted.Decision)
	}
}

// TestAudit_CompressionHint_NeverBypassesHardLimits confirms the hint can
// only move the soft Light/Aggressive threshold -- it must never let a
// step avoid the hard maxCtx/tokenLimit fork-or-critical limits, even with
// "verbose" trying to raise the threshold as far as possible.
func TestAudit_CompressionHint_NeverBypassesHardLimits(t *testing.T) {
	cm := NewContextManager(newHintTestRegistry())
	provider := &MockProvider{name: "gemini"}
	// 1100 tokens: above maxCtx (1000), below tokenLimit (1200) -- must
	// remain LevelCritical/FORCE_FORK regardless of hint.
	text := textOfTokens(1100)

	res, err := cm.Audit(context.Background(), provider, &schemas.CompleteRequest{System: text, CompressionHint: "verbose"})
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelCritical {
		t.Fatalf("expected LevelCritical even with a verbose hint at 1100 tokens (above maxCtx), got %v (action: %s)", res.Decision, res.ActionTaken)
	}
	if res.ActionTaken != "FORCE_FORK" {
		t.Fatalf("expected FORCE_FORK action, got %q", res.ActionTaken)
	}
}
