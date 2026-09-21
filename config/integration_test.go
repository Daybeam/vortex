package config

import (
	"testing"
)

func TestIntegration_FamilyOverrideInRegistry(t *testing.T) {
	// 1. Create a registry with custom providers
	reg := &Registry{
		Providers: map[string]*ProviderConfig{
			"custom_openai": {
				Provider: "openai",
				Model:    "my-special-model", // Auto-detect would fail (default)
				Family:   "deepseek",         // Force deepseek
			},
			"legacy_openai": {
				Provider: "openai",
				Model:    "gpt-4o", // Auto-detect would be "gpt"
			},
		},
		Skills: map[string]*Skill{
			"test_skill": {
				Implementations: map[string]SkillImplementation{
					"gpt":      {SystemPrompt: "GPT_PROMPT"},
					"deepseek": {SystemPrompt: "DEEPSEEK_PROMPT"},
					"default":  {SystemPrompt: "DEFAULT_PROMPT"},
				},
			},
		},
	}

	// 2. Test resolution for "custom_openai" (Should be DEEPSEEK)
	role1 := &Role{Provider: "custom_openai"}
	pc1 := reg.ResolveProviderConfig(role1)
	skill := reg.Skills["test_skill"]

	p1, _ := skill.GetPrompt(pc1.Model, pc1.Family)
	if p1 != "DEEPSEEK_PROMPT" {
		t.Errorf("Custom provider failed: expected DEEPSEEK_PROMPT, got %q (Family was %q)", p1, pc1.Family)
	}

	// 3. Test resolution for "legacy_openai" (Should be GPT)
	role2 := &Role{Provider: "legacy_openai"}
	pc2 := reg.ResolveProviderConfig(role2)

	p2, _ := skill.GetPrompt(pc2.Model, pc2.Family)
	if p2 != "GPT_PROMPT" {
		t.Errorf("Legacy provider failed: expected GPT_PROMPT, got %q (Family was %q)", p2, pc2.Family)
	}
}
