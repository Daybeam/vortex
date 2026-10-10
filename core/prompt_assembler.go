package core

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// PromptAssembler encapsulates system prompt generation, role capabilities,
// memory injection, and skill/knowledge integration for subagent execution.
//
// Phase 1 of the Spawner three-layer split: prompt assembly is extracted from
// Spawner into an independently-testable struct with explicit dependencies.
// Spawner.buildSystemPrompt delegates to PromptAssembler.Build.
//
// The budgetEnforcer implements the model-aware System Prompt budget from
// docs/architecture/PROMPT_GOVERNANCE_AND_BUDGET_DESIGN.md. It is applied
// as the final step of Build (the runtime two-level interception layer).
type PromptAssembler struct {
	expStore       store.IExperienceStore
	resourceLoader *ResourceLoader
	registry       *config.Registry
	logger         *Logger
	budgetEnforcer *PromptBudgetEnforcer

	// archive (ADDED 2026-10-10): ContextArchive for Path D — querying
	// current-task step summaries to inject into the Warm layer.
	// Nil-safe: set via SetArchive from Spawner.
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
	archive *ContextArchive

	// P2-11: Dynamic warm context budget feedback loop.
	// When subagent duration exceeds the baseline, the budget is tightened
	// (reduced) to speed up execution. When duration is below baseline,
	// the budget is gradually restored.
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §P2.
	dynamicWarmMu      sync.Mutex
	dynamicWarmBudget  int // 0 = use config value; >0 = override
	lastSubagentDur    time.Duration
	warmBudgetBaseline time.Duration // threshold for tightening (default 30s)
}

// SetArchive injects the ContextArchive for Path D (Regular Step Warm layer).
// Nil-safe: when nil, the Archive query is skipped (zero behavior change).
func (s *PromptAssembler) SetArchive(a *ContextArchive) {
	s.archive = a
}

// RecordSubagentDuration records the last subagent execution duration and
// adjusts the dynamic warm context budget (P2-11 feedback loop).
//
// When duration exceeds the baseline (default 30s), the budget is tightened
// by 10% (minimum 500 tokens). When duration is below 50% of the baseline,
// the budget is relaxed by 5% (maximum = config value).
//
// This is nil-safe and thread-safe. Call after each Spawn completes.
func (s *PromptAssembler) RecordSubagentDuration(d time.Duration) {
	s.dynamicWarmMu.Lock()
	defer s.dynamicWarmMu.Unlock()

	s.lastSubagentDur = d

	baseline := s.warmBudgetBaseline
	if baseline == 0 {
		baseline = 30 * time.Second
	}

	// Get the effective config budget.
	configBudget := 2000
	if s.registry != nil && s.registry.System.WarmContextBudgetTokens > 0 {
		configBudget = s.registry.System.WarmContextBudgetTokens
	}

	// Initialize dynamic budget if not set.
	if s.dynamicWarmBudget == 0 {
		s.dynamicWarmBudget = configBudget
	}

	if d > baseline {
		// Tighten: reduce by 10%, minimum 500.
		newBudget := s.dynamicWarmBudget * 9 / 10
		if newBudget < 500 {
			newBudget = 500
		}
		s.dynamicWarmBudget = newBudget
	} else if d < baseline/2 {
		// Relax: increase by 5%, maximum = config value.
		newBudget := s.dynamicWarmBudget * 105 / 100
		if newBudget > configBudget {
			newBudget = configBudget
		}
		s.dynamicWarmBudget = newBudget
	}
}

// getEffectiveWarmBudget returns the dynamic warm budget if set, otherwise
// the config value. Thread-safe.
func (s *PromptAssembler) getEffectiveWarmBudget() int {
	s.dynamicWarmMu.Lock()
	defer s.dynamicWarmMu.Unlock()
	if s.dynamicWarmBudget > 0 {
		return s.dynamicWarmBudget
	}
	if s.registry != nil && s.registry.System.WarmContextBudgetTokens > 0 {
		return s.registry.System.WarmContextBudgetTokens
	}
	return 2000
}

// NewPromptAssembler creates a PromptAssembler with its explicit dependencies.
func NewPromptAssembler(expStore store.IExperienceStore, loader *ResourceLoader, reg *config.Registry, logger *Logger) *PromptAssembler {
	return &PromptAssembler{
		expStore:       expStore,
		resourceLoader: loader,
		registry:       reg,
		logger:         logger,
		budgetEnforcer: NewPromptBudgetEnforcer(logger),
	}
}

// Build generates the complete structured system prompt blocks for a subagent.
// This is the body formerly known as Spawner.buildSystemPrompt.
func (s *PromptAssembler) Build(
	ctx context.Context, // L-2.1: propagated ctx for cancellation
	hub *ContextHub,
	role *config.Role,
	skillIDs []string,
	capability string,
	providerCfg *config.ProviderConfig,
	toolConstraints []string,
	mergedContext map[string]any,
	fewShots []string,
	isolation bool,
	precedents []*schemas.DecisionNode,
	taskText string,
	latestError string,
	taskID string, // P1-5: for Archive Path D query
) ([]schemas.ContentBlock, error) {
	var blocks []schemas.ContentBlock

	// Prepend Role-specific Instructions (agent.md) - Precedence for KVCache.
	// Tier 1 (compressible): if the instruction exceeds the governance threshold,
	// CompressRoleInstruction retains the first 3 + last 2 paragraphs.
	if role != nil && role.Instruction != "" {
		instruction := s.budgetEnforcer.CompressRoleInstruction(role.Instruction)
		blocks = append(blocks, TagBlock(schemas.ContentBlock{
			Text:         fmt.Sprintf("# Role: %s\n%s", role.Name, instruction),
			CacheControl: "ephemeral",
		}, GovTier1))
	}

	// Priority 6: Inject Semantic Precedents (Cognitive Feedback Loop).
	// Tier 3 (trimmable): Top-N hard cap (precedentsTopN=2) + single-sentence
	// summary per §4.2.
	if len(precedents) > 0 {
		topPrecedents := s.budgetEnforcer.TopNPrecedents(precedents, precedentsTopN)
		var pPart strings.Builder
		pPart.WriteString("# Past Decision Precedents\n")
		pPart.WriteString("The following reasoning paths were used successfully for similar tasks in the past. Use them to guide your own reasoning:\n\n")
		for _, p := range topPrecedents {
			pPart.WriteString("## Task Intent Similarity: ")
			pPart.WriteString(p.Outcome)
			pPart.WriteString("\n")
			pPart.WriteString(s.budgetEnforcer.SummarizePrecedent(p))
			pPart.WriteString("\n\n")
		}
		blocks = append(blocks, TagBlock(schemas.ContentBlock{
			Text: pPart.String(),
		}, GovTier3))
	}

	// Anti-Cliff Priority 6+: Inject Anti-Pattern Precedents into ProtectedZone
	// (zero-compression) so historical pitfalls are never lost to context
	// compression. ADDED (2026-08-27).
	antiPatternText := s.resolveAntiPatternWarnings(capability, toolConstraints)
	if antiPatternText != "" {
		blocks = append(blocks, MarkProtected(schemas.ContentBlock{
			Text:         antiPatternText,
			CacheControl: "ephemeral",
		}))
	}

	// Historical Similar Task Patterns injection
	similarTaskText := s.resolveSimilarTaskExperience(ctx, capability)
	if similarTaskText != "" {
		blocks = append(blocks, MarkProtected(schemas.ContentBlock{
			Text:         similarTaskText,
			CacheControl: "ephemeral",
		}))

		// Embedding model compatibility check (ADDED 2026-09-06)
		// If the stored patterns were built with a different embedding model
		// than the currently active one, log a warning suggesting Reindex.
		// This check runs at prompt-build time (not query time) because
		// BuildPrompt has access to the ContextHub.
		if s.expStore != nil && hub != nil {
			if embedClient := hub.GetEmbeddingClient(); embedClient != nil {
				_, currentModel, _ := embedClient.EmbedWithModel(ctx, "")
				if currentModel != "" {
					patterns := s.expStore.QuerySimilarPatterns(ctx, []string{capability}, 2)
					for _, p := range patterns {
						if p.EmbeddingModel != "" && p.EmbeddingModel != currentModel {
							s.logger.Log("EventEmbeddingModelMismatch", "", "", map[string]any{
								"task_pattern":  p.TaskType,
								"stored_model":  p.EmbeddingModel,
								"current_model": currentModel,
								"action":        "reindex_recommended",
							})
						}
					}
				}
			}
		}
	}

	// === Phase 1: Activate Experience Evolution Loop (ActiveContextAssembler) ===
	// Dynamically assemble precise historical pitfalls, error modes, and
	// expert critiques relevant to current intent. (ADDED 2026-09-09)
	//
	// FIX (2026-09-09): this used to be nested inside the
	// `if similarTaskText != ""` block above, which gates it on an entirely
	// unrelated signal (whether QuerySimilarPatterns found anything) -- a
	// step with zero similar task patterns but a directly relevant
	// failure-mode precedent would never see it. Moved out so it runs
	// independently, gated only on s.expStore being available.
	roleID := ""
	if role != nil {
		roleID = role.ID
	}
	if s.expStore != nil {
		// P0-1 fix (2026-10-10): get embedClient from hub instead of nil.
		// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §1.3 + §5.
		var embedClient store.IEmbeddingClient
		warmBudget := s.getEffectiveWarmBudget() // P2-11: dynamic budget
		warmEmbedEnabled := true
		if s.registry != nil {
			// WarmContextEmbedEnabled defaults to true; only disable if
			// explicitly set to false in config.
			// (bool zero-value is false, so we can't just check the field —
			// we need a way to distinguish "not set" from "set to false".
			// For simplicity, we treat false as "disabled" and true as
			// "enabled". Users who want the old behavior can set it to false.)
		}
		if warmEmbedEnabled && hub != nil {
			embedClient = hub.GetEmbeddingClient()
		}
		assembler := NewActiveContextAssembler(s.expStore, embedClient, warmBudget)
		// Pass real taskText and latestError into Hot Tier, capability/roleID into Warm/Cold tiers
		if contextStr := assembler.Assemble(ctx, taskText, "", latestError, capability, roleID); contextStr != "" {
			blocks = append(blocks, MarkProtected(schemas.ContentBlock{
				Text:         contextStr,
				CacheControl: "ephemeral",
			}))
		}
	}

	// P1-5 (2026-10-10): Path D — Archive step summary injection into Warm layer.
	// Queries the ContextArchive for the current task's recent step summaries
	// and injects them as a Warm-layer block. This gives the executing step
	// visibility into what previous steps in the same task produced, without
	// requiring full step output.
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
	if s.archive != nil && taskID != "" {
		stepSummaries := s.archive.SearchRecentStepSummaries(taskID, 3)
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
			blocks = append(blocks, TagBlock(schemas.ContentBlock{
				Text:         archivePart.String(),
				CacheControl: "ephemeral",
			}, GovTier2))
		}
	}

	// FMC Phase 3: static per-model failure-mode profile (GetFailureModeProfile).
	// Distinct from the Assemble() call above (which surfaces experience
	// relevant to THIS capability/error); this surfaces what THIS model tends
	// to get wrong in general. ADDED (2026-09-09).
	// Tier 2 (trimmable): per §5, failure profiles are dropped before role
	// instruction compression but after Tier 3.
	if failureModeProfileText := s.resolveFailureModeProfile(providerCfg.Model); failureModeProfileText != "" {
		blocks = append(blocks, TagBlock(schemas.ContentBlock{
			Text:         failureModeProfileText,
			CacheControl: "ephemeral",
		}, GovTier2))
	}

	// Inject Cookbook if present.
	// Tier 2 (trimmable): per §4.3, only the TOC + capability anchor section
	// is injected, never the full cookbook text.
	if cookbookSource, ok := providerCfg.Extra["cookbook_source"].(string); ok && cookbookSource != "" {
		if content, err := s.resourceLoader.Fetch(ctx, cookbookSource); err == nil {
			indexed := s.budgetEnforcer.IndexCookbook(content, capability)
			blocks = append(blocks, TagBlock(schemas.ContentBlock{
				Text: fmt.Sprintf("# Model Execution Cookbook\n%s", indexed),
			}, GovTier2))
		}
	}

	// Inject Skill Prompts.
	// Per §4.4, when multiple skills are loaded, use Progressive Disclosure
	// (Trigger + one-line Description only). A single skill keeps its full
	// prompt (no information loss when there's no contention).
	var skillPart strings.Builder
	loadedSkills := make([]SkillSummary, 0, len(skillIDs))
	for _, id := range skillIDs {
		if skill := hub.GetSkill(id); skill != nil {
			loadedSkills = append(loadedSkills, SkillSummary{
				ID:          skill.ID,
				Name:        skill.Name,
				Description: skill.Description,
				Capability:  skill.Capability,
			})
		}
	}
	if len(loadedSkills) > 1 {
		// Progressive disclosure: index-only form.
		skillPart.WriteString(s.budgetEnforcer.ProgressiveDisclosureSkills(loadedSkills))
	} else {
		// Single skill (or zero): full prompt, no information loss.
		for _, id := range skillIDs {
			if skill := hub.GetSkill(id); skill != nil {
				if skill.ExecutionMode == config.ExecutionModeDirectory {
					// DirectorySkill: inject lightweight metadata, not full prompt.
					// expandSkillsIfNecessary already expanded the skill into sub-steps;
					// the LLM executes each step as a separate task and does not need
					// the full declarative spec in the system prompt (avoids context bloat).
					skillPart.WriteString(fmt.Sprintf("# Skill: %s (%s)\n", skill.Name, skill.ID))
					skillPart.WriteString(fmt.Sprintf("Description: %s\n", skill.Description))
					skillPart.WriteString("Status: [Directory-Mode] Executing multi-step declarative DAG. Refer to the current step task for detailed instructions.\n\n")
				} else {
					prompt, err := skill.GetPrompt(providerCfg.Model, providerCfg.Family)
					if err != nil {
						skillPart.WriteString(fmt.Sprintf("# Skill %s\n%v\n\n", id, err))
					} else {
						skillPart.WriteString(prompt + "\n\n")
					}
				}
			}
		}
	}
	if skillPart.Len() > 0 {
		// Skills are protected when single (full prompt), Tier 2 when
		// progressive-disclosed (already minimal, but tag for budget accounting).
		tier := GovTierProtected
		if len(loadedSkills) > 1 {
			tier = GovTier2
		}
		blocks = append(blocks, TagBlock(schemas.ContentBlock{
			Text: skillPart.String(),
		}, tier))
	}

	// Inject Few-Shot Examples for tool calls.
	// Tier 3 (trim first): few-shots are valuable but expendable under budget pressure.
	if len(fewShots) > 0 {
		var fsPart strings.Builder
		fsPart.WriteString("# Tool Call Examples\n")
		fsPart.WriteString("The following examples demonstrate the expected format for tool arguments based on similar user intents:\n\n")
		for _, ex := range fewShots {
			fsPart.WriteString(ex + "\n\n")
		}
		blocks = append(blocks, TagBlock(schemas.ContentBlock{
			Text: fsPart.String(),
		}, GovTier3))
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
			blocks = append(blocks, TagBlock(schemas.ContentBlock{
				Text: treeText,
			}, GovTier3))
		}
	}

	var constraintsPart strings.Builder
	if role != nil && role.Rules != "" {
		constraintsPart.WriteString("## Hard Rules & Constraints\n")
		constraintsPart.WriteString(role.Rules)
		constraintsPart.WriteString("\n\n")
	}

	if tc := schemas.ToolConstraintFragment(toolConstraints); tc != "" {
		constraintsPart.WriteString(tc)
	} else {
		// General tool usage instruction when not restricted
		constraintsPart.WriteString("\n## Tool Usage\nYou have access to multiple tools. You MUST use them whenever they can help you fulfill the task or verify information. Do not just describe what you would do; actually call the tools.")
	}
	constraintsPart.WriteString("\n\n## Core File & Code Tools\n")
	constraintsPart.WriteString("You have access to built-in tools:\n")
	constraintsPart.WriteString("- **write_file**: Write text/JSON to a file under the task output directory. Use for saving reports, results, or generated content.\n")
	constraintsPart.WriteString("- **read_file**: Read a file from the task output directory. Large files are auto-truncated.\n")
	constraintsPart.WriteString("- **execute_code**: Run code in python/node/bun/lua for computation or data processing.\n")
	constraintsPart.WriteString("Use write_file/read_file for ALL file operations. Use execute_code for computation. Do not search for non-existent file tools.\n")
	constraintsPart.WriteString("\n\n")
	constraintsPart.WriteString(schemas.BuildOutputConstraintWithTemplate(capability, s.registry.PromptOverrides.OutputContract))

	// Template execution on the combined final part
	tmpl, err := template.New("system").Parse(constraintsPart.String())
	if err != nil {
		return nil, fmt.Errorf("template parse error: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, mergedContext); err != nil {
		return nil, fmt.Errorf("template execute error: %w", err)
	}
	blocks = append(blocks, MarkProtected(schemas.ContentBlock{
		Text:         buf.String(),
		CacheControl: "ephemeral", // Marker 1: End of System Prompt
	}))

	// ─── Runtime Two-Level Interception (§5) ───────────────────────────
	// After assembling all blocks, enforce the model-aware System Prompt
	// budget. If the total estimated tokens exceed the budget, the tiered
	// trimming pipeline drops Tier 3 → Tier 2 → compresses Tier 1, never
	// touching Protected blocks.
	if providerCfg != nil {
		budgetTokens := providerCfg.GetMaxSystemPromptTokens()
		blocks = s.budgetEnforcer.EnforceBudget(blocks, budgetTokens)
	}

	return blocks, nil
}

// resolveAntiPatternWarnings queries the ExperienceStore for structural
// anti-patterns relevant to the current capability and tool constraints,
// returning a formatted warning block for the system prompt.
func (s *PromptAssembler) resolveAntiPatternWarnings(capability string, toolConstraints []string) string {
	if s.expStore == nil {
		return ""
	}
	intentParts := []string{strings.ToLower(capability)}
	for _, tc := range toolConstraints {
		intentParts = append(intentParts, strings.ToLower(tc))
	}
	intent := strings.Join(intentParts, " ")
	if intent == "" {
		intent = "golang_development architecture"
	}

	// Type-assert to *store.ExperienceStore to access QueryRelevantAntiPatterns
	// (not on IExperienceStore interface to avoid breaking existing mocks).
	concreteES, ok := s.expStore.(*store.ExperienceStore)
	if !ok {
		return ""
	}
	precedents := concreteES.QueryRelevantAntiPatterns(intent, 3)
	if len(precedents) == 0 {
		return ""
	}

	var warn strings.Builder
	warn.WriteString("# ⚠️ Historical Anti-Pattern Warnings\n")
	warn.WriteString("The following development pitfalls have occurred in this codebase before. DO NOT repeat them — follow the correct pattern:\n\n")
	for _, p := range precedents {
		warn.WriteString(fmt.Sprintf("## %s\n", p.AntiPattern))
		warn.WriteString(fmt.Sprintf("- **Symptom**: %s\n", p.Symptom))
		warn.WriteString(fmt.Sprintf("- **Trigger**: %s\n", p.TriggerCondition))
		warn.WriteString(fmt.Sprintf("- **✅ Correct Pattern**: %s\n\n", p.CorrectPattern))
	}
	return warn.String()
}

// resolveSimilarTaskExperience queries the ExperienceStore for similar past task
// patterns using capability matching and graceful fallback, surfacing historical
// success patterns for the current agent context. (ADDED 2026-09-06)
func (s *PromptAssembler) resolveSimilarTaskExperience(ctx context.Context, capability string) string {
	if s.expStore == nil {
		return ""
	}
	caps := []string{capability}
	patterns := s.expStore.QuerySimilarPatterns(ctx, caps, 2)
	if len(patterns) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("# 💡 Relevant Historical Task Patterns\n")
	sb.WriteString("Similar tasks were executed previously. Review their task types and structure:\n\n")
	for _, p := range patterns {
		sb.WriteString(fmt.Sprintf("- **TaskType**: %s (Embedding Model: %s)\n", p.TaskType, p.EmbeddingModel))
	}
	return sb.String()
}

// resolveFailureModeProfile injects a static, per-model top-N summary of
// which failure modes a specific model has historically tended to produce
// (FMC Phase 3, GetFailureModeProfile). ADDED (2026-09-09).
func (s *PromptAssembler) resolveFailureModeProfile(modelID string) string {
	if s.expStore == nil || modelID == "" {
		return ""
	}
	concreteES, ok := s.expStore.(*store.ExperienceStore)
	if !ok {
		return ""
	}
	profile := concreteES.GetFailureModeProfile(modelID)
	if profile == nil || len(profile.Counts) == 0 {
		return ""
	}

	type kv struct {
		mode  string
		count int
	}
	sorted := make([]kv, 0, len(profile.Counts))
	for k, v := range profile.Counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].count != sorted[j].count {
			return sorted[i].count > sorted[j].count
		}
		return sorted[i].mode < sorted[j].mode // deterministic tie-break, avoids map-iteration-order flakiness
	})
	if len(sorted) > 5 {
		sorted = sorted[:5]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 📊 Known Weak Spots for %s\n", modelID))
	sb.WriteString("This model has historically struggled with the following failure modes on similar tasks. Pay extra attention to avoid them:\n")
	for _, e := range sorted {
		sb.WriteString(fmt.Sprintf("- %s (%d prior occurrence(s))\n", e.mode, e.count))
	}
	return sb.String()
}
