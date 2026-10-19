package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// capturingMockProvider captures the User prompt sent to Complete.
type capturingMockProvider struct {
	capturedUser string
}

func (m *capturingMockProvider) Name() string { return "capture-mock" }
func (m *capturingMockProvider) Complete(_ context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	m.capturedUser = req.User
	return &schemas.ProviderResponse{Text: "tag1,tag2"}, nil
}
func (m *capturingMockProvider) StreamComplete(_ context.Context, req schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	m.capturedUser = req.User
	return &schemas.ProviderResponse{Text: "tag1,tag2"}, nil
}
func (m *capturingMockProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (m *capturingMockProvider) CountTokens(_ context.Context, _ string) (int, error)  { return 0, nil }

// TestReflectionPrompt_ScrubsSecretsFromTrace verifies that ScrubSecrets
// catches API keys in the exact prompt format used by CrystallizeStepToSkill
// and mergeVariants. This is a unit test for the scrubbing at the reflection
// call sites — the full CrystallizeStepToSkill flow requires too many wired
// dependencies (expStore, jit) to mock cleanly.
func TestReflectionPrompt_ScrubsSecretsFromTrace(t *testing.T) {
	// Use 20+ chars after sk- to match the regex pattern sk-[a-zA-Z0-9]{20,}
	secret := "sk-abcDef1234567890XYZ12"

	// CrystallizeStepToSkill prompt format (reflection.go ~line 430)
	traceJSON := fmt.Sprintf(`{"input":"process with %s","output":"key=%s"}`, secret, secret)
	user := fmt.Sprintf(`Task: %s
Capability: %s
Trace:
%s

Goal: Write a Lua script.`, "task with "+secret, "test_cap", traceJSON)

	scrubbed := ScrubSecrets(user)
	if strings.Contains(scrubbed, secret) {
		t.Errorf("secret leaked through ScrubSecrets in CrystallizeStepToSkill prompt format")
	}
	if !strings.Contains(scrubbed, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in scrubbed CrystallizeStepToSkill prompt")
	}

	// mergeVariants prompt format (reflection.go ~line 602)
	variant := fmt.Sprintf("-- Variant: v1\nlocal key = \"%s\"", secret)
	user2 := fmt.Sprintf("Capability: %s\n\nVariants to merge:\n\n%s", "test_cap", variant)

	scrubbed2 := ScrubSecrets(user2)
	if strings.Contains(scrubbed2, secret) {
		t.Errorf("secret leaked through ScrubSecrets in mergeVariants prompt format")
	}
}

// TestCapabilityNavigator_ScrubsSecretsFromQuery is the regression test
// for C6.4: the capability navigator must scrub secrets from the user query
// before sending to the LLM for classification.
func TestCapabilityNavigator_ScrubsSecretsFromQuery(t *testing.T) {
	providers.ClearCache()
	defer providers.ClearCache()

	mock := &capturingMockProvider{}

	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"default": {
				Provider:  "nav-scrub-test",
				BaseURL:   "http://localhost",
				APIKeyEnv: "NAV_SCRUB_KEY",
				Model:     "test-model",
			},
		},
		Roles: map[string]*config.Role{
			"agent": {
				ID:             "agent",
				BaseCapability: "general",
			},
		},
		DefaultProvider: "default",
	}

	cfg := reg.Providers["default"]
	providers.GlobalRouter.Register("nav-scrub-slot", cfg, mock)

	nav := NewCapabilityNavigator(reg)

	// Query contains an API key that should be scrubbed.
	queryWithSecret := "search for api_key=sk-abcDef1234567890XYZ12 in records"

	nav.ClassifyIntent(context.Background(), queryWithSecret)

	if mock.capturedUser == "" {
		t.Fatal("provider was not called — mock setup may have failed")
	}

	secret := "sk-abcDef1234567890XYZ12"
	if strings.Contains(mock.capturedUser, secret) {
		t.Errorf("secret %q leaked into capability classification prompt.\nPrompt: %s", secret, mock.capturedUser)
	}
}
