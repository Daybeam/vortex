package config

import (
	"strings"
	"testing"
)

// ─── GetModelTier ────────────────────────────────────────────────────────

func TestGetModelTier_Flagship(t *testing.T) {
	cases := []string{
		"claude-3-5-sonnet-20241022",
		"claude-3.5-sonnet",
		"claude-sonnet-4-20250514",
		"claude-3-opus-20240229",
		"gpt-4o",
		"gpt-4o-2024-08-06",
		"gpt-4-turbo",
		"deepseek-v3",
		"deepseek-chat",
		"deepseek-r1",
		"gemini-1.5-pro",
		"gemini-2.5-pro",
		"o1-preview",
		"o3-mini",
		"llama-3.1-405b",
		"qwen-2.5-72b-instruct",
	}
	for _, model := range cases {
		pc := &ProviderConfig{Model: model}
		if tier := pc.GetModelTier(); tier != ModelTierFlagship {
			t.Errorf("GetModelTier(%q) = %q, want %q", model, tier, ModelTierFlagship)
		}
	}
}

func TestGetModelTier_Standard(t *testing.T) {
	cases := []string{
		"gpt-4o-mini",
		"gpt-4-mini",
		"gpt-3.5-turbo",
		"claude-3-5-haiku-20241022",
		"claude-3.5-haiku",
		"gemini-1.5-flash",
		"gemini-2.0-flash",
		"deepseek-v2",
		"command-r",
		"command-r-plus",
	}
	for _, model := range cases {
		pc := &ProviderConfig{Model: model}
		if tier := pc.GetModelTier(); tier != ModelTierStandard {
			t.Errorf("GetModelTier(%q) = %q, want %q", model, tier, ModelTierStandard)
		}
	}
}

func TestGetModelTier_Light(t *testing.T) {
	cases := []string{
		"llama-3-8b-instruct",
		"llama-3.1-8b",
		"llama-3.2-1b",
		"llama-3.2-3b",
		"qwen-7b",
		"qwen2.5-7b",
		"qwen2.5-3b",
		"qwen2.5-1.5b",
		"qwen2.5-0.5b",
		"gemma-2b",
		"gemma-7b",
		"gemma2-2b",
		"mistral-7b",
		"phi-3-mini",
		"tinyllama-1.1b",
		// Ollama-style "model:size" (colon → hyphen normalization)
		"qwen2.5:0.5b",
		"qwen2.5:1.5b",
		"qwen2.5:3b",
		"qwen2.5:7b",
		"qwen3:8b",
		"llama3.1:8b",
		"llama3.2:3b",
		"gemma2:2b",
		"mistral:7b",
	}
	for _, model := range cases {
		pc := &ProviderConfig{Model: model}
		if tier := pc.GetModelTier(); tier != ModelTierLight {
			t.Errorf("GetModelTier(%q) = %q, want %q", model, tier, ModelTierLight)
		}
	}
}

func TestGetModelTier_UnknownDefaultsToStandard(t *testing.T) {
	// Unknown/empty models fall back to Standard (not Light) to avoid
	// over-trimming a potentially-flagship model.
	cases := []string{
		"",
		"some-custom-model",
		"my-finetune-v1",
	}
	for _, model := range cases {
		pc := &ProviderConfig{Model: model}
		if tier := pc.GetModelTier(); tier != ModelTierStandard && tier != ModelTierUnknown {
			t.Errorf("GetModelTier(%q) = %q, want Standard or Unknown", model, tier)
		}
	}
	// Empty model → Unknown tier, but budget still falls back to Standard.
	pc := &ProviderConfig{Model: ""}
	if tier := pc.GetModelTier(); tier != ModelTierUnknown {
		t.Errorf("empty model should be Unknown, got %q", tier)
	}
}

func TestGetModelTier_NilSafe(t *testing.T) {
	var pc *ProviderConfig
	if tier := pc.GetModelTier(); tier != ModelTierUnknown {
		t.Errorf("nil ProviderConfig should be Unknown, got %q", tier)
	}
}

func TestGetModelTier_PrefixStripped(t *testing.T) {
	// "models/" and "google/" prefixes should be stripped before matching.
	cases := map[string]ModelTier{
		"models/gpt-4o":         ModelTierFlagship,
		"google/gemini-1.5-pro": ModelTierFlagship,
		"models/gpt-4o-mini":    ModelTierStandard,
		"google/gemma-2b":       ModelTierLight,
	}
	for model, want := range cases {
		pc := &ProviderConfig{Model: model}
		if got := pc.GetModelTier(); got != want {
			t.Errorf("GetModelTier(%q) = %q, want %q", model, got, want)
		}
	}
}

// ─── GetMaxSystemPromptTokens / GetMaxSystemPromptChars ──────────────────

func TestGetMaxSystemPromptTokens(t *testing.T) {
	cases := map[string]int{
		"claude-3.5-sonnet": DefaultSystemPromptBudgetFlagship,
		"gpt-4o":            DefaultSystemPromptBudgetFlagship,
		"gpt-4o-mini":       DefaultSystemPromptBudgetStandard,
		"claude-3.5-haiku":  DefaultSystemPromptBudgetStandard,
		"llama-3-8b":        DefaultSystemPromptBudgetLight,
		"qwen-7b":           DefaultSystemPromptBudgetLight,
		"unknown-model":     DefaultSystemPromptBudgetStandard,
	}
	for model, want := range cases {
		pc := &ProviderConfig{Model: model}
		if got := pc.GetMaxSystemPromptTokens(); got != want {
			t.Errorf("GetMaxSystemPromptTokens(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestGetMaxSystemPromptTokens_ExpectedValues(t *testing.T) {
	if DefaultSystemPromptBudgetLight != 4000 {
		t.Errorf("Light budget = %d, want 4000", DefaultSystemPromptBudgetLight)
	}
	if DefaultSystemPromptBudgetStandard != 8000 {
		t.Errorf("Standard budget = %d, want 8000", DefaultSystemPromptBudgetStandard)
	}
	if DefaultSystemPromptBudgetFlagship != 12000 {
		t.Errorf("Flagship budget = %d, want 12000", DefaultSystemPromptBudgetFlagship)
	}
}

func TestGetMaxSystemPromptChars(t *testing.T) {
	pc := &ProviderConfig{Model: "gpt-4o"}
	want := DefaultSystemPromptBudgetFlagship * CharsPerToken
	if got := pc.GetMaxSystemPromptChars(); got != want {
		t.Errorf("GetMaxSystemPromptChars() = %d, want %d", got, want)
	}
}

// ─── ValidateRoleInstruction ─────────────────────────────────────────────

func TestValidateRoleInstruction_ShortPasses(t *testing.T) {
	role := &Role{ID: "r1", Instruction: "You are a helpful assistant."}
	if w := ValidateRoleInstruction(role); w != nil {
		t.Errorf("short instruction should not warn, got: %v", w)
	}
}

func TestValidateRoleInstruction_EmptyPasses(t *testing.T) {
	role := &Role{ID: "r1"}
	if w := ValidateRoleInstruction(role); w != nil {
		t.Errorf("empty instruction should not warn, got: %v", w)
	}
}

func TestValidateRoleInstruction_NilSafe(t *testing.T) {
	if w := ValidateRoleInstruction(nil); w != nil {
		t.Errorf("nil role should not warn, got: %v", w)
	}
}

func TestValidateRoleInstruction_LongWarns(t *testing.T) {
	longInstruction := strings.Repeat("This is a long instruction paragraph. ", 100)
	role := &Role{ID: "r1", Instruction: longInstruction}
	w := ValidateRoleInstruction(role)
	if w == nil {
		t.Fatal("long instruction should warn")
	}
	if w.RoleID != "r1" {
		t.Errorf("warning RoleID = %q, want r1", w.RoleID)
	}
	if w.Length != len(longInstruction) {
		t.Errorf("warning Length = %d, want %d", w.Length, len(longInstruction))
	}
	if w.Cap != MaxRoleInstructionChars {
		t.Errorf("warning Cap = %d, want %d", w.Cap, MaxRoleInstructionChars)
	}
	if w.Suggestion == "" {
		t.Error("warning should have a non-empty Suggestion")
	}
}

func TestValidateRoleInstruction_Boundary(t *testing.T) {
	// Exactly at the cap → no warning.
	role := &Role{ID: "r1", Instruction: strings.Repeat("a", MaxRoleInstructionChars)}
	if w := ValidateRoleInstruction(role); w != nil {
		t.Errorf("instruction at exactly the cap should not warn, got: %v", w)
	}
	// One char over → warning.
	role.Instruction = strings.Repeat("a", MaxRoleInstructionChars+1)
	if w := ValidateRoleInstruction(role); w == nil {
		t.Error("instruction one char over the cap should warn")
	}
}

// ─── ValidateAllRoleInstructions ─────────────────────────────────────────

func TestValidateAllRoleInstructions_Mixed(t *testing.T) {
	roles := map[string]*Role{
		"clean":   {ID: "clean", Instruction: "short"},
		"bloated": {ID: "bloated", Instruction: strings.Repeat("x", MaxRoleInstructionChars+100)},
		"empty":   {ID: "empty"},
	}
	warnings := ValidateAllRoleInstructions(roles)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if warnings[0].RoleID != "bloated" {
		t.Errorf("warning should be for 'bloated', got %q", warnings[0].RoleID)
	}
}

func TestValidateAllRoleInstructions_AllClean(t *testing.T) {
	roles := map[string]*Role{
		"r1": {ID: "r1", Instruction: "short"},
		"r2": {ID: "r2", Instruction: "also short"},
	}
	if warnings := ValidateAllRoleInstructions(roles); len(warnings) != 0 {
		t.Errorf("expected 0 warnings for clean roles, got %d", len(warnings))
	}
}

func TestValidateAllRoleInstructions_Empty(t *testing.T) {
	if warnings := ValidateAllRoleInstructions(nil); len(warnings) != 0 {
		t.Errorf("expected 0 warnings for nil roles, got %d", len(warnings))
	}
	if warnings := ValidateAllRoleInstructions(map[string]*Role{}); len(warnings) != 0 {
		t.Errorf("expected 0 warnings for empty roles, got %d", len(warnings))
	}
}

// ─── RoleInstructionWarning.String ───────────────────────────────────────

func TestRoleInstructionWarning_String(t *testing.T) {
	w := RoleInstructionWarning{
		RoleID:     "r1",
		Length:     3000,
		Cap:        2000,
		Suggestion: "split it",
	}
	s := w.String()
	if !strings.Contains(s, "r1") {
		t.Errorf("String() should contain RoleID, got: %s", s)
	}
	if !strings.Contains(s, "3000") {
		t.Errorf("String() should contain Length, got: %s", s)
	}
	if !strings.Contains(s, "2000") {
		t.Errorf("String() should contain Cap, got: %s", s)
	}
}

// ─── GetReserveTokens ────────────────────────────────────────────────────

func TestGetReserveTokens_TierAwareDefaults(t *testing.T) {
	cases := map[string]int{
		"llama-3.1-8b":      DefaultReserveTokensLight,
		"qwen2.5-3b":        DefaultReserveTokensLight,
		"gpt-4o-mini":       DefaultReserveTokensStandard,
		"claude-3.5-haiku":  DefaultReserveTokensStandard,
		"claude-3.5-sonnet": DefaultReserveTokensFlagship,
		"gpt-4o":            DefaultReserveTokensFlagship,
		"unknown-model":     DefaultReserveTokensStandard,
	}
	for model, want := range cases {
		pc := &ProviderConfig{Model: model}
		if got := pc.GetReserveTokens(); got != want {
			t.Errorf("GetReserveTokens(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestGetReserveTokens_ExpectedValues(t *testing.T) {
	if DefaultReserveTokensLight != 2048 {
		t.Errorf("Light reserve = %d, want 2048", DefaultReserveTokensLight)
	}
	if DefaultReserveTokensStandard != 4096 {
		t.Errorf("Standard reserve = %d, want 4096", DefaultReserveTokensStandard)
	}
	if DefaultReserveTokensFlagship != 4096 {
		t.Errorf("Flagship reserve = %d, want 4096", DefaultReserveTokensFlagship)
	}
}

func TestGetReserveTokens_ExplicitOverride(t *testing.T) {
	pc := &ProviderConfig{Model: "gpt-4o", ReserveTokens: 8192}
	if got := pc.GetReserveTokens(); got != 8192 {
		t.Errorf("explicit override: got %d, want 8192", got)
	}
}

func TestGetReserveTokens_ZeroMeansDefault(t *testing.T) {
	pc := &ProviderConfig{Model: "llama-3.1-8b", ReserveTokens: 0}
	if got := pc.GetReserveTokens(); got != DefaultReserveTokensLight {
		t.Errorf("zero ReserveTokens should fall back to tier default, got %d, want %d", got, DefaultReserveTokensLight)
	}
}

func TestGetReserveTokens_NegativeMeansDefault(t *testing.T) {
	pc := &ProviderConfig{Model: "gpt-4o", ReserveTokens: -100}
	if got := pc.GetReserveTokens(); got != DefaultReserveTokensFlagship {
		t.Errorf("negative ReserveTokens should fall back to tier default, got %d, want %d", got, DefaultReserveTokensFlagship)
	}
}

func TestGetReserveTokens_NilSafe(t *testing.T) {
	var pc *ProviderConfig
	if got := pc.GetReserveTokens(); got != DefaultReserveTokensStandard {
		t.Errorf("nil ProviderConfig should return Standard default, got %d", got)
	}
}
