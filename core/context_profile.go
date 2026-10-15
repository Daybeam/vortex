package core

import (
	"sort"

	"github.com/daybeam/vortex/schemas"
)

// ContextProfile is the token-proportion breakdown of a system prompt, used by
// the observability UI to visualize how the System Prompt budget is spent.
//
// It is computed from the *post-budget* blocks produced by Build, so categories
// reflect what was actually sent to the model (after tiered trimming /
// compression) — not the untrimmed ideal. This is the real data the
// /api/context/profile endpoint surfaces: skills / mcp / rules / precedents
// all appear as labeled categories.
type ContextProfile struct {
	// Model / Provider identify which model tier the budget figures apply to.
	Model    string `json:"model"`
	Provider string `json:"provider"`

	// SystemTokens is the total estimated tokens of the assembled (post-budget)
	// system prompt — the sum of all category tokens.
	SystemTokens int `json:"system_tokens"`
	// BudgetTokens is the model-aware System Prompt token budget.
	BudgetTokens int `json:"budget_tokens"`
	// ReserveTokens is the reply reserve for the model tier.
	ReserveTokens int `json:"reserve_tokens"`
	// BlockCount is the number of content blocks that survived budget trimming.
	BlockCount int `json:"block_count"`

	// Outer ring (ADDED 2026-10-12): the Categories above only cover the
	// system ring. The rest of the context actually sent to the model is
	// composed of the MCP tool schemas and the conversation history, plus
	// the provider-reported prompt/completion token counts of the last
	// turn. These are folded in by the spawner after each successful
	// provider call (see PromptAssembler.UpdateOuterRing) so the
	// /api/context/profile donut shows the full context budget.
	ToolsTokens      int `json:"tools_tokens"`
	HistoryTokens    int `json:"history_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`

	// Categories holds per-category token sums (treemap rows). Sorted by
	// tokens descending for deterministic rendering.
	Categories []CategoryToken `json:"categories"`
}

// CategoryToken is a single treemap row: one semantic category of the system
// prompt, its governance tier, and its estimated token cost.
type CategoryToken struct {
	Category string `json:"category"`
	Tier     string `json:"tier"`
	Tokens   int    `json:"tokens"`
}

// ComputeContextProfile aggregates the token cost of each semantic category
// across the (already assembled + budget-enforced) blocks. It reuses the same
// 4-chars/token heuristic the budget enforcer uses, so the SystemTokens figure
// is consistent with the budget comparison in prompt_budget.go.
func (s *PromptAssembler) ComputeContextProfile(blocks []schemas.ContentBlock, budgetTokens, reserveTokens int, model, provider string) *ContextProfile {
	profile := &ContextProfile{
		Model:         model,
		Provider:      provider,
		BudgetTokens:  budgetTokens,
		ReserveTokens: reserveTokens,
		BlockCount:    len(blocks),
	}
	agg := make(map[string]*CategoryToken)
	order := make([]string, 0, len(blocks))
	for i := range blocks {
		b := blocks[i]
		cat := b.Category
		if cat == "" {
			cat = "uncategorized"
		}
		tier, _ := b.Metadata[GovernanceMarker].(string)
		ct, ok := agg[cat]
		if !ok {
			ct = &CategoryToken{Category: cat, Tier: tier}
			agg[cat] = ct
			order = append(order, cat)
		}
		tok := s.budgetEnforcer.EstimateTokens(b.Text)
		ct.Tokens += tok
		profile.SystemTokens += tok
	}
	// Stable, deterministic order: by tokens desc, then category name.
	cats := make([]CategoryToken, 0, len(order))
	for _, cat := range order {
		cats = append(cats, *agg[cat])
	}
	sort.Slice(cats, func(i, j int) bool {
		if cats[i].Tokens != cats[j].Tokens {
			return cats[i].Tokens > cats[j].Tokens
		}
		return cats[i].Category < cats[j].Category
	})
	profile.Categories = cats
	return profile
}

// LastProfile returns the most recently captured system-prompt profile, or nil
// if Build has not run yet. Safe for concurrent use.
func (s *PromptAssembler) LastProfile() *ContextProfile {
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	return s.lastProfile
}

// setLastProfile stores the most recent profile. Safe for concurrent use.
func (s *PromptAssembler) setLastProfile(p *ContextProfile) {
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	s.lastProfile = p
}

// UpdateOuterRing folds the per-turn outer-ring composition into the most
// recently captured profile (ADDED 2026-10-12). The system ring is frozen at
// Build() time; the outer ring is per-turn, so the spawner calls this after
// each successful provider call with:
//   - tools: estimated token cost of the MCP tool schemas sent to the model
//   - history: estimated token cost of the conversation-history blocks
//   - prompt / completion: provider-reported token counts of the last turn
//
// No-op when no profile has been captured yet. Safe for concurrent use.
func (s *PromptAssembler) UpdateOuterRing(tools, history, prompt, completion int) {
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	if s.lastProfile == nil {
		return
	}
	s.lastProfile.ToolsTokens = tools
	s.lastProfile.HistoryTokens = history
	s.lastProfile.PromptTokens = prompt
	s.lastProfile.CompletionTokens = completion
}
