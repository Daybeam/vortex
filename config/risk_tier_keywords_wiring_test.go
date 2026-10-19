// Regression tests for the critical_risk_keywords / moderate_risk_keywords
// config wiring (2026-10-18).
//
// Bug being pinned: the two config fields were consumed by
//
//	schemas.SetRiskTierKeywords(r.System.CriticalRiskKeywords, ...)
//
// which ignores empty slices ("empty = keep existing"). Combined with the
// `json:"...,omitempty"` tags, the config surface could only ADD keywords,
// never remove one. A deployment therefore could not switch off the default
// "payment" keyword — and since tools/tools_task.go appends an auditor DAG
// step to every RiskTierCritical step, any task whose text mentioned payment
// silently grew one extra LLM call that the operator had no way to disable.
//
// These tests drive the real loader so they fail if the wiring regresses.

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// restoreDefaultRiskKeywords puts the process-global keyword lists back to the
// engine defaults. The keyword config is package-global in schemas, so tests
// that mutate it must not leak into each other.
func restoreDefaultRiskKeywords() {
	schemas.SetRiskTierKeywords(
		[]string{"browser", "image", "generate", "scrape", "screenshot", "pollinations", "deploy", "payment"},
		[]string{"patch", "refactor", "build", "test"},
	)
}

// loadConfigWith writes raw JSON to a temp config file and loads it through
// the production path (NewRegistry + Load), so the assertions exercise
// loader.go rather than calling setters directly.
func loadConfigWith(t *testing.T, raw string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config_risktier.json")
	if err := os.WriteFile(configPath, []byte(raw), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	r, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestCriticalRiskKeywords_EmptyArrayDisablesTier is the core regression:
// `[]` must mean "turn the tier off", not "keep defaults".
func TestCriticalRiskKeywords_EmptyArrayDisablesTier(t *testing.T) {
	t.Cleanup(restoreDefaultRiskKeywords)

	loadConfigWith(t, `{"system":{"critical_risk_keywords":[]}}`)

	if tier, _ := schemas.DetermineRiskTier("send a payment reminder", nil); tier != schemas.RiskTierLight {
		t.Fatalf("`critical_risk_keywords: []` must disable the tier; 'payment' still classified %s", tier)
	}
	if tier, _ := schemas.DetermineRiskTier("deploy to production", nil); tier != schemas.RiskTierLight {
		t.Fatalf("`critical_risk_keywords: []` must disable the tier; 'deploy' still classified %s", tier)
	}
}

// TestCriticalRiskKeywords_AbsentKeepsDefaults makes sure the disable path did
// not accidentally become the default. Configs that never mention the field
// must behave byte-identically to before.
func TestCriticalRiskKeywords_AbsentKeepsDefaults(t *testing.T) {
	t.Cleanup(restoreDefaultRiskKeywords)

	loadConfigWith(t, `{"system":{}}`)

	if tier, _ := schemas.DetermineRiskTier("send a payment reminder", nil); tier != schemas.RiskTierCritical {
		t.Fatalf("absent field must keep engine defaults; 'payment' classified %s", tier)
	}
}

// TestCriticalRiskKeywords_ExplicitListReplacesDefaults verifies replacement
// still works (the original feature) and is not a merge.
func TestCriticalRiskKeywords_ExplicitListReplacesDefaults(t *testing.T) {
	t.Cleanup(restoreDefaultRiskKeywords)

	loadConfigWith(t, `{"system":{"critical_risk_keywords":["transfer_funds"]}}`)

	if tier, _ := schemas.DetermineRiskTier("transfer_funds now", nil); tier != schemas.RiskTierCritical {
		t.Fatalf("explicit keyword must be critical, got %s", tier)
	}
	if tier, _ := schemas.DetermineRiskTier("send a payment reminder", nil); tier != schemas.RiskTierLight {
		t.Fatalf("explicit list must REPLACE defaults; 'payment' still classified %s", tier)
	}
}

// TestModerateRiskKeywords_EmptyArrayDisablesTier covers the second field so
// the two halves cannot drift.
func TestModerateRiskKeywords_EmptyArrayDisablesTier(t *testing.T) {
	t.Cleanup(restoreDefaultRiskKeywords)

	loadConfigWith(t, `{"system":{"moderate_risk_keywords":[]}}`)

	if tier, _ := schemas.DetermineRiskTier("refactor the auth module", nil); tier != schemas.RiskTierLight {
		t.Fatalf("`moderate_risk_keywords: []` must disable the tier; 'refactor' still %s", tier)
	}
	// Critical tier untouched.
	if tier, _ := schemas.DetermineRiskTier("send a payment reminder", nil); tier != schemas.RiskTierCritical {
		t.Fatalf("clearing moderate must not affect critical, got %s", tier)
	}
}

// TestCriticalRiskKeywords_EmptySurvivesRoundTrip guards the `omitempty`
// removal: with omitempty present, an empty slice marshals to nothing, so a
// persisted config would lose the operator's "disabled" intent on the next
// Load(). This asserts the field is emitted.
func TestCriticalRiskKeywords_EmptySurvivesRoundTrip(t *testing.T) {
	var s SystemSettings
	s.CriticalRiskKeywords = []string{}
	s.ModerateRiskKeywords = nil

	if s.CriticalRiskKeywords == nil {
		t.Fatalf("precondition: empty slice must stay non-nil")
	}
	// Go's encoding/json without omitempty emits `"critical_risk_keywords":[]`.
	// A nil field emits `null`, which the loader treats as absent.
	blob, err := jsonMarshalIndent(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !containsKey(blob, `"critical_risk_keywords": []`) && !containsKey(blob, `"critical_risk_keywords":[]`) {
		t.Fatalf("empty critical_risk_keywords must be serialised (no omitempty), got:\n%s", blob)
	}
}

// ── small helpers so the test file does not drag encoding/json into the
// package under test's import surface beyond what it already has ────────────

func jsonMarshalIndent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}

func containsKey(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
