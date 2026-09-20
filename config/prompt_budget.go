package config

import (
	"fmt"
	"strings"
)

// ─── Model-Aware System Prompt Budget ─────────────────────────────────────
//
// Design ref: docs/architecture/PROMPT_GOVERNANCE_AND_BUDGET_DESIGN.md §3.
//
// Different model tiers have wildly different context windows, attention
// decay profiles, and tokenizer characteristics. A single fixed System
// Prompt budget either starves flagship models or overwhelms lightweight
// ones. GetMaxSystemPromptTokens returns a tier-appropriate budget based on
// the ProviderConfig.Model string.

type ModelTier string

const (
	ModelTierLight    ModelTier = "light"    // < ~8B params / local
	ModelTierStandard ModelTier = "standard" // 8B-70B / mid-tier
	ModelTierFlagship ModelTier = "flagship" // 70B+ / frontier
	ModelTierUnknown  ModelTier = "unknown"  // unrecognized — caller falls back
)

const (
	// DefaultSystemPromptBudgetLight is the budget for lightweight/local models
	// (Llama-3-8B, Qwen-7B, …). ~16,000 chars at 4 chars/token.
	DefaultSystemPromptBudgetLight = 4000
	// DefaultSystemPromptBudgetStandard is the budget for mid-tier models
	// (GPT-4o-mini, Claude-3.5-Haiku, …). ~32,000 chars.
	DefaultSystemPromptBudgetStandard = 8000
	// DefaultSystemPromptBudgetFlagship is the budget for frontier models
	// (Claude 3.5 Sonnet, GPT-4o, DeepSeek-V3, …). ~48,000 chars.
	DefaultSystemPromptBudgetFlagship = 12000

	// DefaultReserveTokensLight is the reply reserve for lightweight/local
	// models with small context windows (4K-8K). Smaller reserve because
	// the window itself is tight and reply space needs are modest.
	DefaultReserveTokensLight = 2048
	// DefaultReserveTokensStandard is the reply reserve for mid-tier models
	// (8K-128K context). 4096 covers most tool-call reply scenarios.
	DefaultReserveTokensStandard = 4096
	// DefaultReserveTokensFlagship is the reply reserve for frontier models
	// (128K-200K context). Same as Standard — large windows don't reduce
	// the reply space requirement.
	DefaultReserveTokensFlagship = 4096

	// CharsPerToken is the rough UTF-8 chars-per-token heuristic used to
	// convert between token budgets and char budgets. English/code averages
	// ~4; CJK can be ~1-2. We use the conservative 4× to avoid over-budgeting.
	CharsPerToken = 4

	// MaxRoleInstructionChars is the soft cap above which a Role.Instruction
	// emits a config-time Warning. Per §4.1, this catches LLM-generated or
	// hand-written instructions that have grown bloated and are eating
	// front-loaded attention budget.
	MaxRoleInstructionChars = 2000
)

// GetModelTier classifies the provider's Model string into a budget tier.
// Detection is conservative: unknown models fall through to Standard rather
// than Light, because misclassifying a flagship as Light would over-trim
// and misclassifying a lightweight as Flagship would under-trim — the
// former causes visible prompt corruption while the latter only causes
// budget waste.
//
// Detection order matters: Standard and Light markers are checked BEFORE
// Flagship markers because flagship names are often substrings of mid-tier
// names (e.g. "gpt-4o" is contained in "gpt-4o-mini"). Checking the more
// specific markers first prevents misclassification.
func (p *ProviderConfig) GetModelTier() ModelTier {
	if p == nil || p.Model == "" {
		return ModelTierUnknown
	}
	m := strings.ToLower(p.Model)
	m = strings.TrimPrefix(m, "models/")
	m = strings.TrimPrefix(m, "google/")
	// Normalize Ollama-style "model:size" → "model-size" so that
	// existing hyphen-based markers (e.g. "qwen2.5-7b") also match
	// Ollama-served models (e.g. "qwen2.5:7b").
	m = strings.ReplaceAll(m, ":", "-")

	// Lightweight / local: Llama-3-8B, Qwen-7B, Gemma-2B/7B, Mistral-7B,
	// Phi-3, small local fine-tunes. Check first — these are the most
	// specific small-model names and won't accidentally match larger ones.
	lightMarkers := []string{
		"llama-3-8b", "llama-3.1-8b", "llama-3.2-1b", "llama-3.2-3b",
		"llama3-8b", "llama3.1-8b", "llama3.2-1b", "llama3.2-3b", // Ollama
		"qwen-7b", "qwen2.5-7b", "qwen2.5-3b", "qwen2.5-1.5b", "qwen2.5-0.5b",
		"qwen3-8b", "qwen-1.8b", "qwen-0.5b",
		"gemma-2b", "gemma-7b", "gemma2-2b", "gemma2-9b",
		"mistral-7b", "phi-3", "phi-2", "tinyllama", "yi-6b",
	}
	for _, marker := range lightMarkers {
		if strings.Contains(m, marker) {
			return ModelTierLight
		}
	}

	// Standard: GPT-4o-mini, Claude-3.5-Haiku, Gemini-1.5-Flash, etc.
	// Check before flagship — "gpt-4o-mini" contains "gpt-4o", so the
	// standard marker must be tested first to win.
	standardMarkers := []string{
		"gpt-4o-mini", "gpt-4-mini", "gpt-3.5",
		"claude-3.5-haiku", "claude-3-haiku", "claude-haiku",
		"claude-3-5-haiku", // hyphen-variant of 3.5
		"gemini-1.5-flash", "gemini-flash", "gemini-2-flash", "gemini-2.0-flash",
		"deepseek-v2", "command-r",
	}
	for _, marker := range standardMarkers {
		if strings.Contains(m, marker) {
			return ModelTierStandard
		}
	}

	// Flagship / frontier: Claude 3.5 Sonnet/Opus, GPT-4o, GPT-4-Turbo,
	// DeepSeek-V3, Gemini-1.5-Pro, Llama-3.1-405B, Qwen-2.5-72B.
	// Both dot and hyphen variants are included (claude-3.5-sonnet and
	// claude-3-5-sonnet).
	flagshipMarkers := []string{
		"claude-3-opus", "claude-3.5-sonnet", "claude-3-5-sonnet",
		"claude-3-sonnet", "claude-sonnet", "claude-opus", "claude-4",
		"gpt-4o", "gpt-4-turbo", "gpt-4-0", "gpt-4.1", "o1-", "o3-",
		"deepseek-v3", "deepseek-chat", "deepseek-r1",
		"gemini-1.5-pro", "gemini-2.0-pro", "gemini-2.5-pro", "gemini-pro",
		"llama-3.1-405b", "llama-3.1-70b", "llama-3.3-70b",
		"qwen-2.5-72b", "qwen3-72b", "qwen-72b",
	}
	for _, marker := range flagshipMarkers {
		if strings.Contains(m, marker) {
			return ModelTierFlagship
		}
	}

	// Default to Standard for unrecognized models (see comment above).
	return ModelTierStandard
}

// GetMaxSystemPromptTokens returns the System Prompt token budget for this
// provider's model tier. Returns DefaultSystemPromptBudgetStandard for
// nil/unknown models so callers always get a finite, safe budget.
func (p *ProviderConfig) GetMaxSystemPromptTokens() int {
	tier := p.GetModelTier()
	switch tier {
	case ModelTierLight:
		return DefaultSystemPromptBudgetLight
	case ModelTierStandard:
		return DefaultSystemPromptBudgetStandard
	case ModelTierFlagship:
		return DefaultSystemPromptBudgetFlagship
	default:
		return DefaultSystemPromptBudgetStandard
	}
}

// GetMaxSystemPromptChars returns the System Prompt char budget
// (tokens × CharsPerToken). Convenience for char-based truncation paths.
func (p *ProviderConfig) GetMaxSystemPromptChars() int {
	return p.GetMaxSystemPromptTokens() * CharsPerToken
}

// GetReserveTokens returns the reply reserve token budget for this provider.
// The reserve is the token space set aside for the model's reply — the
// context window's effective input capacity is (MaxContextWindow - reserve).
//
// If ReserveTokens is explicitly configured (> 0), that value is used.
// Otherwise, a tier-aware default is returned based on GetModelTier():
// Light → 2048, Standard/Flagship → 4096. This prevents the context from
// being filled to 100% of the window, leaving no space for the model to
// generate a reply.
//
// Design ref: docs/architecture/CONTEXT_WINDOW_RESERVE_DESIGN.md §3.3.
func (p *ProviderConfig) GetReserveTokens() int {
	if p != nil && p.ReserveTokens > 0 {
		return p.ReserveTokens
	}
	tier := p.GetModelTier()
	switch tier {
	case ModelTierLight:
		return DefaultReserveTokensLight
	case ModelTierStandard:
		return DefaultReserveTokensStandard
	case ModelTierFlagship:
		return DefaultReserveTokensFlagship
	default:
		return DefaultReserveTokensStandard
	}
}

// RoleInstructionWarning is returned by ValidateRoleInstruction when a
// Role's Instruction exceeds MaxRoleInstructionChars. The caller (loader)
// logs it as a Warning but does not reject the config — the runtime
// PromptBudgetEnforcer handles structural compression.
type RoleInstructionWarning struct {
	RoleID     string
	Length     int
	Cap        int
	Suggestion string
}

func (w RoleInstructionWarning) String() string {
	return fmt.Sprintf(
		"role %q instruction is %d chars (cap %d): %s",
		w.RoleID, w.Length, w.Cap, w.Suggestion,
	)
}

// ValidateRoleInstruction checks a single Role's Instruction length against
// MaxRoleInstructionChars. Returns a *RoleInstructionWarning if exceeded,
// nil otherwise. Per §4.1, this is a config-time Warning, not an error.
func ValidateRoleInstruction(role *Role) *RoleInstructionWarning {
	if role == nil || role.Instruction == "" {
		return nil
	}
	if len(role.Instruction) <= MaxRoleInstructionChars {
		return nil
	}
	return &RoleInstructionWarning{
		RoleID:     role.ID,
		Length:     len(role.Instruction),
		Cap:        MaxRoleInstructionChars,
		Suggestion: "consider splitting into Rules + a shorter Instruction; the runtime will compress but explicit structure is preferred",
	}
}

// ValidateAllRoleInstructions runs ValidateRoleInstruction over every Role
// in the Registry and returns all warnings. The loader calls this after a
// successful load and logs each warning. Returns nil if all roles are clean.
func ValidateAllRoleInstructions(roles map[string]*Role) []RoleInstructionWarning {
	if len(roles) == 0 {
		return nil
	}
	var warnings []RoleInstructionWarning
	for _, role := range roles {
		if w := ValidateRoleInstruction(role); w != nil {
			warnings = append(warnings, *w)
		}
	}
	return warnings
}
