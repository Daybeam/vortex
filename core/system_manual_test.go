package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// testRegistry builds a minimal Registry with enough data to exercise every
// manual section. Values are deliberately non-default so a regression that
// swaps them for zero values is visible.
func testRegistry() *config.Registry {
	return &config.Registry{
		DefaultProvider: "openai",
		Providers: map[string]*config.ProviderConfig{
			"openai": {
				Model:      "gpt-4",
				APIKeyEnv:  "OPENAI_API_KEY",
				BaseURL:    "https://api.openai.com/v1",
				APIKey:     "sk-secret-do-not-leak",
			},
			"local": {
				Model:         "llama3",
				FallbackChain: []config.ProviderConfig{{Provider: "openai"}},
			},
		},
		Roles: map[string]*config.Role{
			"researcher": {Instruction: "Find and synthesize information from sources."},
			"coder":      {Instruction: "Write clean, tested code."},
		},
		Skills: map[string]*config.Skill{
			"summarize": {},
		},
		MCPs: map[string]*config.MCPDef{
			"filesystem": {},
		},
		GlobalFallback: []config.ProviderConfig{
			{Provider: "local", Model: "llama3"},
		},
		System: config.SystemSettings{
			MaxToolTurns:          30,
			MaxSpawnDepth:         3,
			SandboxedMemoryMB:     512,
			SandboxedCPUSecs:      10,
			DelegationMode:        true,
			NonInteractive:        false,
			ConfidenceThreshold:   0.7,
		},
	}
}

func TestBuildSystemManual_NilRegistry(t *testing.T) {
	got := BuildSystemManual(nil, "overview", "admin")
	if !strings.Contains(got, "unavailable") {
		t.Errorf("nil registry should return unavailable message, got: %s", got)
	}
}

func TestBuildSystemManual_DefaultTopicIsOverview(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "", "admin")
	if !strings.Contains(got, "Orchestrator System Overview") {
		t.Errorf("empty topic should default to overview, got: %s", got)
	}
}

func TestBuildSystemManual_UnknownTopic(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "nonexistent", "admin")
	if !strings.Contains(got, "Unknown topic") {
		t.Errorf("unknown topic should return error message, got: %s", got)
	}
}

func TestBuildSystemManual_OverviewContainsCapabilities(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "overview", "admin")
	for _, want := range []string{"role", "skill", "MCP", "provider", "delegate_to_orchestrator"} {
		if !strings.Contains(got, want) {
			t.Errorf("overview should mention %q, got: %s", want, got)
		}
	}
}

func TestBuildSystemManual_ToolsListsAllChatTools(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "tools", "admin")
	for _, want := range []string{
		"read_file", "write_file", "execute_code",
		"delegate_to_orchestrator", "orchestrator_task_status", "orchestrator_help",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tools section should list %q, got: %s", want, got)
		}
	}
}

func TestBuildSystemManual_ConfigGuestBlocked(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "config", "guest")
	if !strings.Contains(got, "not available for guest") {
		t.Errorf("guest should be blocked from config, got: %s", got)
	}
}

func TestBuildSystemManual_ConfigAuthorizedShowsProvidersNotLimits(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "config", "authorized")
	// Authorized sees providers...
	if !strings.Contains(got, "openai") {
		t.Errorf("authorized should see provider names, got: %s", got)
	}
	// ...but not admin-only system settings (limits).
	if strings.Contains(got, "Max tool turns") {
		t.Errorf("authorized should NOT see admin-only system settings, got: %s", got)
	}
}

func TestBuildSystemManual_ConfigAdminSeesEverything(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "config", "admin")
	for _, want := range []string{"openai", "Max tool turns", "System Settings", "Global Fallback"} {
		if !strings.Contains(got, want) {
			t.Errorf("admin should see %q in config, got: %s", want, got)
		}
	}
}

func TestBuildSystemManual_NoSecretsLeaked(t *testing.T) {
	reg := testRegistry()
	// The test registry has a real-looking API key. It must NEVER appear in
	// any manual output, regardless of topic or tier. This is the core safety
	// property: the system manual is served to the chat model, which relays
	// to users — a leaked key would be a critical vulnerability.
	secret := "sk-secret-do-not-leak"
	for _, topic := range []string{"overview", "tools", "config", "safety"} {
		for _, tier := range []string{"guest", "authorized", "admin"} {
			got := BuildSystemManual(reg, topic, tier)
			if strings.Contains(got, secret) {
				t.Errorf("SECRET LEAK: topic=%q tier=%q output contains API key %q", topic, tier, secret)
			}
		}
	}
}

func TestBuildSystemManual_AdminConfigShowsAPIKeyEnvNotKey(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "config", "admin")
	// Admin sees the env var NAME (for debugging config), not the key value.
	if !strings.Contains(got, "api_key_env=OPENAI_API_KEY") {
		t.Errorf("admin should see api_key_env name, got: %s", got)
	}
	if strings.Contains(got, "sk-secret") {
		t.Errorf("admin must NOT see the actual API key value, got: %s", got)
	}
}

func TestBuildSystemManual_SafetyContainsHostProtection(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "safety", "guest")
	for _, want := range []string{"Workspace isolation", "Sandboxed execution", "Host Damage Prevention"} {
		if !strings.Contains(got, want) {
			t.Errorf("safety section should mention %q, got: %s", want, got)
		}
	}
}

func TestBuildSystemManual_SafetyGuestNoAdminSection(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "safety", "guest")
	if strings.Contains(got, "Admin-Only Information") {
		t.Errorf("guest should not see admin-only safety section, got: %s", got)
	}
	if !strings.Contains(got, "Guest/Authorized Limitations") {
		t.Errorf("guest should see guest limitations section, got: %s", got)
	}
}

func TestBuildSystemManual_SafetyAdminSeesAdminSection(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "safety", "admin")
	if !strings.Contains(got, "Admin-Only Information") {
		t.Errorf("admin should see admin-only safety section, got: %s", got)
	}
}

func TestBuildSystemManual_TierNormalization(t *testing.T) {
	reg := testRegistry()
	// Uppercase / whitespace should normalize correctly.
	got := BuildSystemManual(reg, "config", "  ADMIN  ")
	if !strings.Contains(got, "System Settings") {
		t.Errorf("tier '  ADMIN  ' should normalize to admin and see settings, got: %s", got)
	}
	got = BuildSystemManual(reg, "config", "GUEST")
	if !strings.Contains(got, "not available for guest") {
		t.Errorf("tier 'GUEST' should normalize to guest and be blocked, got: %s", got)
	}
}

func TestBuildSystemManual_ToolsGuestNoRoles(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "tools", "guest")
	if strings.Contains(got, "Available Roles") {
		t.Errorf("guest should not see role details in tools section, got: %s", got)
	}
}

func TestBuildSystemManual_ToolsAuthorizedSeesRoles(t *testing.T) {
	reg := testRegistry()
	got := BuildSystemManual(reg, "tools", "authorized")
	if !strings.Contains(got, "Available Roles") {
		t.Errorf("authorized should see role details in tools section, got: %s", got)
	}
	if !strings.Contains(got, "researcher") {
		t.Errorf("authorized should see role names, got: %s", got)
	}
}
