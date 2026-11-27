package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// buildKnowledgeBlocks renders model failure guidance, the indexed cookbook,
// skill prompts, few-shot examples, and the collapsed tree path, in that order.
func (s *PromptAssembler) buildKnowledgeBlocks(
	ctx context.Context,
	hub *ContextHub,
	skillIDs []string,
	capability string,
	providerCfg *config.ProviderConfig,
	fewShots []string,
	mergedContext map[string]any,
	isolation bool,
) []schemas.ContentBlock {
	var blocks []schemas.ContentBlock

	// FMC Phase 3: static per-model failure-mode profile (GetFailureModeProfile).
	// Distinct from the Assemble() call above (which surfaces experience
	// relevant to THIS capability/error); this surfaces what THIS model tends
	// to get wrong in general. ADDED (2026-09-09).
	// Tier 2 (trimmable): per §5, failure profiles are dropped before role
	// instruction compression but after Tier 3.
	if failureModeProfileText := s.resolveFailureModeProfile(providerCfg.Model); failureModeProfileText != "" {
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text:         failureModeProfileText,
			CacheControl: "ephemeral",
		}, GovTier2, CategoryFailureProfile))
	}

	// Inject Cookbook if present.
	// Tier 2 (trimmable): per §4.3, only the TOC + capability anchor section
	// is injected, never the full cookbook text.
	if cookbookSource, ok := providerCfg.Extra["cookbook_source"].(string); ok && cookbookSource != "" {
		if content, err := s.resourceLoader.Fetch(ctx, cookbookSource); err == nil {
			indexed := s.budgetEnforcer.IndexCookbook(content, capability)
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text: fmt.Sprintf("# Model Execution Cookbook\n%s", indexed),
			}, GovTier2, CategoryCookbook))
		}
	}

	// Inject Skill Prompts. See buildSkillPromptBlocks for the progressive
	// disclosure rules.
	blocks = append(blocks, s.buildSkillPromptBlocks(hub, skillIDs, providerCfg)...)

	// Inject Few-Shot Examples for tool calls.
	// Tier 3 (trim first): few-shots are valuable but expendable under budget pressure.
	if len(fewShots) > 0 {
		var fsPart strings.Builder
		fsPart.WriteString("# Tool Call Examples\n")
		fsPart.WriteString("The following examples demonstrate the expected format for tool arguments based on similar user intents:\n\n")
		for _, ex := range fewShots {
			fsPart.WriteString(ex + "\n\n")
		}
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text: fsPart.String(),
		}, GovTier3, CategoryFewShot))
	}

	// Inject Tree Context.
	// Tier 3 (trim first): per §4.5, apply a sliding window — the most recent
	// treePathActiveWindow Active/Running nodes stay fully expanded; older
	// resolved nodes collapse to single-line ASSERT statements.
	if path, ok := mergedContext["path"].([]map[string]any); ok && len(path) > 0 && !isolation {
		archetype := providerCfg.Archetype
		if archetype == "" {
			archetype = config.ArchetypeAR
		}

		windowedPath := s.budgetEnforcer.SlidingWindowTreePath(path)
		treeText := s.budgetEnforcer.RenderCollapsedTreePath(windowedPath, archetype)
		if treeText != "" {
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text: treeText,
			}, GovTier3, CategoryTreePath))
		}
	}

	return blocks
}
