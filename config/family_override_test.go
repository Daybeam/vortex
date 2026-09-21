package config

import (
	"testing"
)

func TestDetectFamily(t *testing.T) {
	tests := []struct {
		modelID  string
		expected string
	}{
		{"gpt-4o", "gpt"},
		{"claude-3-5-sonnet", "claude"},
		{"gemini-1.5-pro", "gemini"},
		{"models/gemini-1.5-flash", "gemini"},
		{"google/gemini-2.0-flash-exp", "gemini"},
		{"deepseek-chat", "deepseek"},
		{"sensenova-6.7-flash", "gpt"},
		{"sentimes-deepseek-v4", "gpt"},
		{"glm-4", "gpt"},
		{"unknown-model", "default"},
	}

	for _, tt := range tests {
		got := detectFamily(tt.modelID)
		if got != tt.expected {
			t.Errorf("detectFamily(%q) = %q; want %q", tt.modelID, got, tt.expected)
		}
	}
}

func TestSkill_GetPrompt_Override(t *testing.T) {
	skill := &Skill{
		ID: "test_skill",
		Implementations: map[string]SkillImplementation{
			"gpt":      {SystemPrompt: "gpt prompt"},
			"deepseek": {SystemPrompt: "deepseek prompt"},
			"default":  {SystemPrompt: "default prompt"},
		},
	}

	// 1. Automatic detection
	p1, _ := skill.GetPrompt("gpt-4o")
	if p1 != "gpt prompt" {
		t.Errorf("Auto detection failed: expected 'gpt prompt', got %q", p1)
	}

	// 2. Explicit override (Same as detected)
	p2, _ := skill.GetPrompt("gpt-4o", "gpt")
	if p2 != "gpt prompt" {
		t.Errorf("Override with same value failed: expected 'gpt prompt', got %q", p2)
	}

	// 3. Explicit override (Different from detected)
	// Even if model name looks like GPT, we force DeepSeek prompt
	p3, _ := skill.GetPrompt("gpt-4o", "deepseek")
	if p3 != "deepseek prompt" {
		t.Errorf("Override with different value failed: expected 'deepseek prompt', got %q", p3)
	}

	// 4. Unknown model with override
	p4, _ := skill.GetPrompt("my-custom-model", "gpt")
	if p4 != "gpt prompt" {
		t.Errorf("Override for unknown model failed: expected 'gpt prompt', got %q", p4)
	}
}

func TestRegistry_ResolveCookbookSource_Override(t *testing.T) {
	reg := &Registry{
		RoleCookbookSources: map[string]string{
			"gpt":      "http://gpt.cookbook",
			"deepseek": "http://deepseek.cookbook",
			"default":  "http://default.cookbook",
		},
	}

	// 1. Automatic detection
	c1 := reg.ResolveCookbookSource("gpt-4o")
	if c1 != "http://gpt.cookbook" {
		t.Errorf("Auto detection failed: expected 'http://gpt.cookbook', got %q", c1)
	}

	// 2. Explicit override
	c2 := reg.ResolveCookbookSource("gpt-4o", "deepseek")
	if c2 != "http://deepseek.cookbook" {
		t.Errorf("Override failed: expected 'http://deepseek.cookbook', got %q", c2)
	}
}
