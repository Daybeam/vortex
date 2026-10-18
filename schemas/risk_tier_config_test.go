package schemas

import (
	"testing"
)

// TestDetermineRiskTier_DefaultKeywords verifies the default keyword lists
// preserve the original hardcoded behavior (backward compat).
func TestDetermineRiskTier_DefaultKeywords(t *testing.T) {
	tests := []struct {
		task     string
		mcpList  []string
		wantTier RiskTier
	}{
		{"send a payment reminder", nil, RiskTierCritical},
		{"deploy to production", nil, RiskTierCritical},
		{"take a screenshot of the page", nil, RiskTierCritical},
		{"refactor the auth module", nil, RiskTierModerate},
		{"run the test suite", nil, RiskTierModerate},
		{"read a file", nil, RiskTierLight},
		{"", nil, RiskTierLight},
	}
	for _, tt := range tests {
		tier, _ := DetermineRiskTier(tt.task, tt.mcpList)
		if tier != tt.wantTier {
			t.Errorf("DetermineRiskTier(%q) = %s, want %s", tt.task, tier, tt.wantTier)
		}
	}
}

// TestSetRiskTierKeywords_Override verifies custom keywords override defaults.
func TestSetRiskTierKeywords_Override(t *testing.T) {
	original, _ := DetermineRiskTier("payment", nil)
	if original != RiskTierCritical {
		t.Fatalf("precondition: expected critical for 'payment', got %s", original)
	}

	SetRiskTierKeywords([]string{"financial", "transaction"}, []string{"cleanup"})
	defer SetRiskTierKeywords(
		[]string{"browser", "image", "generate", "scrape", "screenshot", "pollinations", "deploy", "payment"},
		[]string{"patch", "refactor", "build", "test"},
	)

	tier, _ := DetermineRiskTier("payment", nil)
	if tier != RiskTierLight {
		t.Errorf("after override, 'payment' should be light, got %s", tier)
	}

	tier, _ = DetermineRiskTier("process a financial transaction", nil)
	if tier != RiskTierCritical {
		t.Errorf("after override, 'financial' should be critical, got %s", tier)
	}

	tier, _ = DetermineRiskTier("cleanup the database", nil)
	if tier != RiskTierModerate {
		t.Errorf("after override, 'cleanup' should be moderate, got %s", tier)
	}
}

// TestSetRiskTierKeywords_EmptyKeepsDefaults verifies empty slices keep defaults.
func TestSetRiskTierKeywords_EmptyKeepsDefaults(t *testing.T) {
	SetRiskTierKeywords(nil, nil)

	tier, _ := DetermineRiskTier("payment", nil)
	if tier != RiskTierCritical {
		t.Errorf("empty override should keep default: 'payment' should be critical, got %s", tier)
	}

	tier, _ = DetermineRiskTier("refactor", nil)
	if tier != RiskTierModerate {
		t.Errorf("empty override should keep default: 'refactor' should be moderate, got %s", tier)
	}
}

// TestClearRiskTierKeywords_DisablesTier pins the only way to turn a default
// keyword off. Before ClearRiskTierKeywords existed, SetRiskTierKeywords(empty,
// empty) was a no-op, so a deployment could NOT disable "payment" — and every
// AppWorld task whose text mentions payment silently got an auditor step
// appended (tools/tools_task.go RiskTierCritical branch), i.e. one extra LLM
// call per task that the caller had no way to switch off.
func TestClearRiskTierKeywords_DisablesTier(t *testing.T) {
	defer SetRiskTierKeywords(
		[]string{"browser", "image", "generate", "scrape", "screenshot", "pollinations", "deploy", "payment"},
		[]string{"patch", "refactor", "build", "test"},
	)

	ClearRiskTierKeywords("critical")

	tier, _ := DetermineRiskTier("send a payment reminder", nil)
	if tier != RiskTierLight {
		t.Errorf("after clearing critical, 'payment' should be light, got %s", tier)
	}
	tier, _ = DetermineRiskTier("deploy to production", nil)
	if tier != RiskTierLight {
		t.Errorf("after clearing critical, 'deploy' should be light, got %s", tier)
	}
	// Moderate tier is untouched.
	tier, _ = DetermineRiskTier("refactor the auth module", nil)
	if tier != RiskTierModerate {
		t.Errorf("clearing critical must not affect moderate, got %s", tier)
	}
}

// TestClearRiskTierKeywords_GhostDriverStillCritical guards the MCP-name
// branch: it is structural (a browser driver is critical by construction) and
// must survive keyword clearing, otherwise a config could silently downgrade
// real browser work to light.
func TestClearRiskTierKeywords_GhostDriverStillCritical(t *testing.T) {
	defer SetRiskTierKeywords(
		[]string{"browser", "image", "generate", "scrape", "screenshot", "pollinations", "deploy", "payment"},
		[]string{"patch", "refactor", "build", "test"},
	)

	ClearRiskTierKeywords("critical")

	tier, _ := DetermineRiskTier("do something ordinary", []string{"ghost-driver"})
	if tier != RiskTierCritical {
		t.Errorf("ghost-driver must stay critical regardless of keywords, got %s", tier)
	}
}
