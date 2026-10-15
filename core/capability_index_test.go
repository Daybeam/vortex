package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestCapabilityIndex_TwoCharTokensNotDropped is a regression test for TODO-2.
// The addTokens helper in Rebuild previously used len(t) >= 3 (byte-based),
// which silently dropped 2-char English/alphanumeric tokens like "AI", "ML",
// "QA", "go", "js" from the Cuckoo filter and tag index. These are common
// capability tags that should be discoverable.
//
// See docs/plan/SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-2.
func TestCapabilityIndex_TwoCharTokensNotDropped(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"ai_skill": {ID: "ai_skill", Capability: "AI"},
			"ml_skill": {ID: "ml_skill", Capability: "ML"},
			"qa_skill": {ID: "qa_skill", Capability: "QA"},
			"go_skill": {ID: "go_skill", Capability: "go"},
			"js_skill": {ID: "js_skill", Capability: "js"},
			"3d_skill": {ID: "3d_skill", Capability: "3d"},
		},
	}

	idx := NewCapabilityIndex(reg)

	twoCharTokens := []string{"ai", "ml", "qa", "go", "js", "3d"}
	for _, tag := range twoCharTokens {
		if !idx.Tags[tag] {
			t.Errorf("2-char token %q should be in Tags index (was dropped by len>=3 threshold)", tag)
		}
		if !idx.Maybe(tag) {
			t.Errorf("2-char token %q should pass Cuckoo Maybe() check", tag)
		}
	}
}

// TestCapabilityIndex_SingleCharTokensDropped verifies that single ASCII
// characters ("a", "I", "x") are still filtered out — they are noise.
func TestCapabilityIndex_SingleCharTokensDropped(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"noise_a": {ID: "noise_a", Capability: "a"},
			"noise_i": {ID: "noise_i", Capability: "I"},
			"noise_x": {ID: "noise_x", Capability: "x"},
		},
	}

	idx := NewCapabilityIndex(reg)

	singleChars := []string{"a", "i", "x"}
	for _, tag := range singleChars {
		if idx.Tags[tag] {
			t.Errorf("single-char token %q should be filtered out (noise)", tag)
		}
	}
}

// TestCapabilityIndex_ChineseTokensPreserved verifies that Chinese tokens
// are not regressed by the TODO-2 fix. Chinese chars are 3 bytes each in
// UTF-8, so even single Chinese chars (3 bytes) pass len >= 2.
//
// This also documents that the design doc's claim about Chinese 2-char words
// being dropped by len>=3 was incorrect — "行情" is 6 bytes, always passed.
func TestCapabilityIndex_ChineseTokensPreserved(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"single_cn": {ID: "single_cn", Capability: "期"},    // 3 bytes, 1 rune
			"double_cn": {ID: "double_cn", Capability: "行情"},   // 6 bytes, 2 runes
			"triple_cn": {ID: "triple_cn", Capability: "期货分析"}, // 12 bytes, 4 runes
		},
	}

	idx := NewCapabilityIndex(reg)

	chineseTokens := []string{"期", "行情", "期货分析"}
	for _, tag := range chineseTokens {
		if !idx.Tags[tag] {
			t.Errorf("Chinese token %q should be in Tags index", tag)
		}
	}
}
