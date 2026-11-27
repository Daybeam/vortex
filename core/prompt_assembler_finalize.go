package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// appendArchiveAndShapeBlocks adds turn-varying context after the stable prefix
// and its volatile experience blocks, preserving the KV-2 ordering contract.
func (s *PromptAssembler) appendArchiveAndShapeBlocks(blocks []schemas.ContentBlock, taskID string) []schemas.ContentBlock {
	// KV-2 (2026-10-25): archive blocks moved to the END of the block list
	// (after all stable blocks) so the stable prefix (role + constraints +
	// tools + few-shots) is byte-identical across turns → prefix cache hits.
	// The archive changes mid-step (Archive.Append during the turn loop) →
	// placing it after the stable blocks prevents it from invalidating the
	// cacheable prefix.
	if s.archive != nil && taskID != "" {
		stepSummaries := s.archive.SearchRecentStepSummaries(taskID, s.ProjectOf(taskID), 3)
		if len(stepSummaries) > 0 {
			var archivePart strings.Builder
			archivePart.WriteString("# Recent Step Summaries (Same Task)\n")
			archivePart.WriteString("The following summaries are from previously completed steps in this task. Use them to understand what has already been done:\n\n")
			for _, sm := range stepSummaries {
				stepLabel := sm.NodeID
				if stepLabel == "" && len(sm.StepIDs) > 0 {
					stepLabel = sm.StepIDs[0]
				}
				if stepLabel == "" {
					stepLabel = "unknown"
				}
				archivePart.WriteString(fmt.Sprintf("- **Step %s**: %s\n", stepLabel, sm.Summary))
			}
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text:         archivePart.String(),
				CacheControl: "ephemeral",
			}, GovTier2, CategoryArchiveSummary))
		}
	}

	blocks = append(blocks, s.shapeMismatchBlock()...)
	return blocks
}

// finalizePromptBlocks applies fold before tiered budget enforcement, then
// captures the actual post-budget token profile. Protected blocks are untouched.
func (s *PromptAssembler) finalizePromptBlocks(blocks []schemas.ContentBlock, providerCfg *config.ProviderConfig, taskID string) []schemas.ContentBlock {
	// ─── Runtime Two-Level Interception (§5) ───────────────────────────
	// After assembling all blocks, enforce the model-aware System Prompt
	// budget. If the total estimated tokens exceed the budget, the tiered
	// trimming pipeline drops Tier 3 → Tier 2 → compresses Tier 1, never
	// touching Protected blocks.
	//
	// Fold engine runs FIRST: it folds sealed segments into summaries
	// (reversible, incremental, semantic boundaries). Then the enforcer
	// handles any remaining overflow via tier drops + hard truncate.
	if providerCfg != nil {
		budgetTokens := providerCfg.GetMaxSystemPromptTokens()

		// Phase 0: Segment-based incremental fold (before budget enforcement).
		if s.foldEngine != nil {
			blocks = s.foldEngine.FoldSealedSegments(blocks, taskID, budgetTokens)
		}

		// Phase 1-4: Tiered budget enforcement (existing, unchanged).
		blocks = s.budgetEnforcer.EnforceBudget(blocks, budgetTokens)
	}

	// ── Token-proportion observability snapshot ───────────────────────────
	// Capture the *actual* (post-budget) system-prompt breakdown so the
	// observability UI can visualize where the System Prompt budget is spent.
	// This is the real data the /api/context/profile endpoint surfaces —
	// skills / mcp / rules / precedents all appear as labeled categories.
	budgetTokens, reserveTokens := 0, 0
	model, provider := "", ""
	if providerCfg != nil {
		budgetTokens = providerCfg.GetMaxSystemPromptTokens()
		reserveTokens = providerCfg.GetReserveTokens()
		model = providerCfg.Model
		provider = providerCfg.Provider
	}
	s.setLastProfile(s.ComputeContextProfile(blocks, budgetTokens, reserveTokens, model, provider))

	return blocks
}
