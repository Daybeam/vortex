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
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
	archive *ContextArchive

	// foldEngine (ADDED 2026-10-13): Segment-Based Incremental Fold engine.
	// Performs reversible compression of sealed segments before budget enforcement.
	// Nil-safe: when nil, fold is skipped (zero behavior change).
	// See docs/plan/SEGMENT_BASED_INCREMENTAL_FOLD_DESIGN.md.
	foldEngine *FoldEngine

	// memoryBankStore (ADDED 2026-10-17): injects UserProfile into sub-agent
	// prompts so spawned agents know user preferences/expertise.
	// Nil-safe: set via SetMemoryBankStore from Spawner.
	// See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
	memoryBankStore *store.MemoryBankStore

	// promptManager (ADDED 2026-10-18): loads external prompt templates
	// from prompts/ dir or remote URL, renders with text/template, caches
	// parsed templates. Used as fallback when role.Instruction is empty.
	// Nil-safe: when nil or template fetch fails, fallback is skipped.
	// See ONION_ARCHITECTURE.md §8 "Wire PromptManager".
	promptManager *PromptManager

	// recentInteractions (ADDED 2026-10-25): recent tool interactions from
	// the current step's trace, used by CheckDataShapeMismatch to detect
	// inconsistent DataShape metadata and warn the model.
	// Nil-safe: when nil/empty, shape mismatch check is skipped.
	// Guarded by recentInteractionsMu — SetRecentInteractions writes from
	// the spawner goroutine while shapeMismatchBlock reads from Build.
	recentInteractionsMu sync.Mutex
	recentInteractions   []store.ToolInteraction

	// P2-11: Dynamic warm context budget feedback loop.
	// When subagent duration exceeds the baseline, the budget is tightened
	// (reduced) to speed up execution. When duration is below baseline,
	// the budget is gradually restored.
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §P2.
	dynamicWarmMu      sync.Mutex
	dynamicWarmBudget  int // 0 = use config value; >0 = override
	lastSubagentDur    time.Duration
	warmBudgetBaseline time.Duration // threshold for tightening (default 30s)

	// lastProfile is the most recent token-proportion snapshot, captured at
	// the end of Build (post-budget enforcement). It reflects the *actual*
	// system prompt that was assembled. Nil until the first Build. Read/write
	// only via LastProfile / setLastProfile (mutex-guarded) because the API
	// endpoint may read it concurrently with a spawn-time write.
	profileMu   sync.Mutex
	lastProfile *ContextProfile
}

// SetArchive injects the ContextArchive for Path D (Regular Step Warm layer).
// Nil-safe: when nil, the Archive query is skipped (zero behavior change).
func (s *PromptAssembler) SetArchive(a *ContextArchive) {
	s.archive = a
}

// SetFoldEngine injects the FoldEngine for segment-based incremental fold.
// Nil-safe: when nil, fold is skipped (zero behavior change).
func (s *PromptAssembler) SetFoldEngine(fe *FoldEngine) {
	s.foldEngine = fe
}

// SetMemoryBankStore injects the MemoryBankStore for UserProfile injection
// into sub-agent prompts. Nil-safe: when nil, no UserProfile is injected.
// ADDED 2026-10-17. See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
func (s *PromptAssembler) SetMemoryBankStore(mb *store.MemoryBankStore) {
	s.memoryBankStore = mb
}

// SetRecentInteractions injects the current step's tool interactions so
// Build can detect DataShape mismatches via CheckDataShapeMismatch.
// Nil-safe: when nil/empty, the shape mismatch check is skipped.
func (s *PromptAssembler) SetRecentInteractions(trace []store.ToolInteraction) {
	s.recentInteractionsMu.Lock()
	s.recentInteractions = trace
	s.recentInteractionsMu.Unlock()
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
		promptManager:  NewPromptManager("prompts", loader),
	}
}

// stablePromptTime returns a turn-stable timestamp for prompt templates. The
// value feeds the cacheable system-prompt PREFIX (base_role.md renders it), so a
// per-call time.Now() changed the prefix on every turn and defeated provider
// prefix caching — the engine's own CacheSentinel names this exact cause
// ("prompt prefix changes every turn (timestamp/random injection) breaking
// cache stability"). The task graph's CreatedAt is set once at submit, so it is
// identical for every turn of every step of the task: a deliberate trade of
// second-level clock accuracy for cache stability. Falls back to time.Now()
// when no graph is available (unchanged behavior).
// @decision:DECISION-KVCACHE-STABLE-TIME
func stablePromptTime(hub *ContextHub) time.Time {
	if hub != nil && hub.Graph != nil && !hub.Graph.CreatedAt.IsZero() {
		return hub.Graph.CreatedAt
	}
	return time.Now()
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
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text:         fmt.Sprintf("# Role: %s\n%s", role.Name, instruction),
			CacheControl: "ephemeral",
		}, GovTier1, CategoryRoleInstruction))
	} else if role != nil && s.promptManager != nil {
		// PromptManager fallback (ADDED 2026-10-18): when a role has no
		// custom instruction, try loading base_role.md template from prompts/.
		// Error-safe: on any failure (file missing, parse error), silently
		// skip — zero behavior change from pre-fallback.
		tmplCtx := map[string]any{
			"task": map[string]any{
				"id":   taskID,
				"role": role.Name,
			},
			"env": map[string]string{
				"Time": stablePromptTime(hub).Format(time.RFC3339),
			},
		}
		if rendered, err := s.promptManager.GetAndRender("base_role.md", tmplCtx); err == nil && rendered != "" {
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text:         rendered,
				CacheControl: "ephemeral",
			}, GovTier1, CategoryRoleInstruction))
		}
	}

	// MemoryBank UserProfile injection (ADDED 2026-10-17).
	// Sub-agents get the same user context as the gateway agent, so they can
	// tailor output to user expertise/preferences. Only UserProfile is injected
	// (not full MemoryBank) to avoid token bloat from ActiveContext/Decisions.
	// See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
	if s.memoryBankStore != nil && s.registry != nil && s.registry.System.MemoryBankEnabled {
		if mb, err := s.memoryBankStore.Load(); err == nil && mb != nil {
			up := mb.UserProfile
			if len(up.Expertise) > 0 || len(up.Preferences) > 0 || up.CommunicationStyle != "" {
				var b strings.Builder
				b.WriteString("\n## User Context\n")
				if len(up.Expertise) > 0 {
					b.WriteString("Expertise: " + strings.Join(up.Expertise, ", ") + "\n")
				}
				if len(up.Preferences) > 0 {
					b.WriteString("Preferences: " + strings.Join(up.Preferences, ", ") + "\n")
				}
				if up.CommunicationStyle != "" {
					b.WriteString("Communication style: " + up.CommunicationStyle + "\n")
				}
				blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
					Text:         b.String(),
					CacheControl: "ephemeral",
				}, GovTier1, CategoryRoleInstruction))
			}
		}
	}

	// Epistemic Grounding Rule (self-healing §2): injected whenever the memory
	// system is active. Tells sub-agents that long-term memory and past
	// experience are heuristics, NOT immutable truth — fresh tool output and
	// current observations always win over cached facts. Prevents "epistemic
	// rigidity" where agents blindly trust outdated API contracts, revoked
	// preferences, or superseded document states.
	if s.registry != nil && s.registry.System.MemoryBankEnabled {
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text: "\n## ⚠️ Epistemic Grounding Rule\n" +
				"Long-term memory and past experience are heuristics, NOT immutable truth. " +
				"If any cached fact regarding external APIs, user constraints, document states, or environment conditions " +
				"conflicts with fresh tool output or current observation, TRUST THE TOOL/OBSERVATION. " +
				"Emit a correction memo if memory is stale.\n",
			CacheControl: "ephemeral",
		}, GovTier1, CategoryRoleInstruction))
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
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text: pPart.String(),
		}, GovTier3, CategoryPastPrecedents))
	}

	// Anti-Cliff Priority 6+: Inject Anti-Pattern Precedents into ProtectedZone
	// (zero-compression) so historical pitfalls are never lost to context
	// compression. ADDED (2026-08-27).
	antiPatternText := s.resolveAntiPatternWarnings(capability, toolConstraints)
	if antiPatternText != "" {
		blocks = append(blocks, MarkProtectedWithCategory(schemas.ContentBlock{
			Text:         antiPatternText,
			CacheControl: "ephemeral",
		}, CategoryAntiPattern))
	}

	// Historical Similar Task Patterns injection
	similarTaskText := s.resolveSimilarTaskExperience(ctx, capability)
	if similarTaskText != "" {
		blocks = append(blocks, MarkProtectedWithCategory(schemas.ContentBlock{
			Text:         similarTaskText,
			CacheControl: "ephemeral",
		}, CategorySimilarTask))

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
		// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §1.3 + §5.
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
			blocks = append(blocks, MarkProtectedWithCategory(schemas.ContentBlock{
				Text:         contextStr,
				CacheControl: "ephemeral",
			}, CategoryActiveContext))
		}
	}

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
						// M2 (2026-10-25): wrap Trial skill prompts in an untrusted_data
						// fence so the model treats the content as unverified (injection
						// defense for pending skills used via session_skills).
						if skill.Trial {
							prompt = fmt.Sprintf("<untrusted_data from=\"trial_skill\" reason=\"pending_verification\">\n%s\n</untrusted_data>", prompt)
						}
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
		blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
			Text: skillPart.String(),
		}, tier, CategorySkills))
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

	var constraintsPart strings.Builder
	if role != nil && role.Rules != "" {
		constraintsPart.WriteString("## Hard Rules & Constraints\n")
		constraintsPart.WriteString(role.Rules)
		constraintsPart.WriteString("\n\n")
	}

	// A role may opt out of the framework's own framing; see Role.MinimalPrompt.
	minimalPrompt := role != nil && role.MinimalPrompt

	if tc := schemas.ToolConstraintFragment(toolConstraints); tc != "" {
		constraintsPart.WriteString(tc)
	} else {
		// General tool usage instruction when not restricted
		constraintsPart.WriteString("\n## Tool Usage\nYou have access to multiple tools. You MUST use them whenever they can help you fulfill the task or verify information. Do not just describe what you would do; actually call the tools.")
	}
	if !minimalPrompt {
		// Both fragments below assert things about the role's surface and its
		// answer shape. They are right for a general executor and wrong for a
		// role that owns its own contract — see Role.MinimalPrompt.
		//
		// The surface fragment is rendered from the SAME policy the dispatcher
		// enforces (coreToolPolicyFromRole; spawner_mcp.go filters schemas with
		// it, spawner_tool_exec.go refuses calls with it). A hardcoded list was
		// wrong the moment appworld_agent set core_tools to read_file/write_file:
		// the prompt kept advertising a code sandbox the role no longer had, and
		// the model — measured 2026-10-19 on 4fab96f_3, 7 of 8 core calls —
		// answered by writing code against a sandbox with no app data instead of
		// calling the MCP tools that did. Advertisement follows capability.
		corePol := coreToolPolicyFromRole(role)
		hasWrite := corePol.allows("write_file")
		hasRead := corePol.allows("read_file")
		hasCode := corePol.allows("execute_code")
		if hasWrite || hasRead || hasCode {
			if hasWrite || hasRead {
				constraintsPart.WriteString("\n\n## Core File & Code Tools\n")
			} else {
				constraintsPart.WriteString("\n\n## Core Code Tool\n")
			}
			constraintsPart.WriteString("You have access to built-in tools:\n")
			if hasWrite {
				constraintsPart.WriteString("- **write_file**: Write text/JSON to a file under the task output directory. Use for saving reports, results, or generated content.\n")
			}
			if hasRead {
				constraintsPart.WriteString("- **read_file**: Read a file from the task output directory. Large files are auto-truncated.\n")
			}
			if hasCode {
				constraintsPart.WriteString("- **execute_code**: Run code in python/node/bun/lua for computation or data processing.\n")
			}
			if hasWrite || hasRead {
				if hasCode {
					constraintsPart.WriteString("Use write_file/read_file for ALL file operations. Use execute_code for computation. Do not search for non-existent file tools.\n")
				} else {
					// Naming the absence is load-bearing, not pedantry: without it a
					// model that wants a computation has no reason to prefer a tool
					// call over inventing a sandbox invocation, and burns its turns
					// on a refusal loop.
					constraintsPart.WriteString("Use write_file/read_file for ALL file operations. Do not search for non-existent file tools.\n")
				}
			}
			// The output directory is per-TASK, not per-step: spawner_tool_exec.go
			// resolves it as filepath.Join(outputBase, req.TaskID), and every step
			// of a DAG shares that TaskID. So it is the default hand-off channel
			// between steps — the one that needs no planner annotation, no
			// InputMapping, and no workspace key. Saying so is load-bearing: a step
			// that does not know a predecessor wrote there will re-derive the same
			// data with the same tools, which is both wasted turns and (measured
			// 2026-10-19, 4fab96f_3) a run that hits MAX_TURNS_EXHAUSTED.
			switch {
			case hasRead && hasWrite:
				constraintsPart.WriteString("This output directory is SHARED by every step of this task. A predecessor step may have written intermediate results here — list it and read those files before re-deriving anything an earlier step already produced. When your own result is large and a later step only needs the conclusion, write the distilled version here instead of making the next step reconstruct it.\n")
			case hasRead:
				constraintsPart.WriteString("This output directory is SHARED by every step of this task. A predecessor step may have written intermediate results here — list it and read those files before re-deriving anything an earlier step already produced.\n")
			case hasWrite:
				constraintsPart.WriteString("This output directory is SHARED by every step of this task, so a later step can read what you write here. When your result is large and a later step only needs the conclusion, write the distilled version here instead of passing the whole thing through your answer.\n")
			}
		} else if corePol.restricted {
			// Restricted to nothing: say so once, without a heading, so the model
			// does not spend a turn looking for tools that were deliberately
			// withheld. This is the planner's shape.
			constraintsPart.WriteString("\n\nThere are no built-in file or code tools available to you in this role. Everything you know must come from the tools listed above.\n")
		}
		constraintsPart.WriteString("\n\n")
		constraintsPart.WriteString(schemas.BuildOutputConstraintWithTemplate(capability, s.registry.PromptOverrides.OutputContract))
	} else {
		constraintsPart.WriteString("\n\n")
	}

	// Template execution on the combined final part
	tmpl, err := template.New("system").Parse(constraintsPart.String())
	if err != nil {
		return nil, fmt.Errorf("template parse error: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, mergedContext); err != nil {
		return nil, fmt.Errorf("template execute error: %w", err)
	}
	blocks = append(blocks, MarkProtectedWithCategory(schemas.ContentBlock{
		Text:         buf.String(),
		CacheControl: "ephemeral", // Marker 1: End of System Prompt
	}, CategoryConstraints))

	// ─── Runtime Two-Level Interception (§5) ───────────────────────────
	// After assembling all blocks, enforce the model-aware System Prompt
	// budget. If the total estimated tokens exceed the budget, the tiered
	// KV-2 (2026-10-25): archive blocks moved to the END of the block list
	// (after all stable blocks) so the stable prefix (role + constraints +
	// tools + few-shots) is byte-identical across turns → prefix cache hits.
	// The archive changes mid-step (Archive.Append during the turn loop) →
	// placing it after the stable blocks prevents it from invalidating the
	// cacheable prefix.
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
			blocks = append(blocks, TagBlockWithCategory(schemas.ContentBlock{
				Text:         archivePart.String(),
				CacheControl: "ephemeral",
			}, GovTier2, CategoryArchiveSummary))
		}
	}

	blocks = append(blocks, s.shapeMismatchBlock()...)

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

// shapeMismatchBlock returns a Tier-2 warning block if recent tool
// interactions have inconsistent DataShape metadata, or nil otherwise.
func (s *PromptAssembler) shapeMismatchBlock() []schemas.ContentBlock {
	s.recentInteractionsMu.Lock()
	trace := s.recentInteractions
	s.recentInteractionsMu.Unlock()
	if len(trace) == 0 {
		return nil
	}
	warn := CheckDataShapeMismatch(trace)
	if warn == "" {
		return nil
	}
	return []schemas.ContentBlock{TagBlockWithCategory(schemas.ContentBlock{
		Text:         warn,
		CacheControl: "ephemeral",
	}, GovTier2, CategoryShapeMismatch)}
}

// CheckDataShapeMismatch checks if recent tool interactions have inconsistent
// DataShape metadata. If two interactions have different DataShape values,
// returns a warning string that should be injected into the prompt to prevent
// the model from blindly merging incompatible data.
//
// See docs/plan/STRUCTURED_TOOL_METADATA_DESIGN.md §2.3.
func CheckDataShapeMismatch(interactions []store.ToolInteraction) string {
	seen := make(map[string]bool)
	var shapes []string
	for _, it := range interactions {
		if it.Meta == nil || it.Meta.DataShape == "" {
			continue
		}
		if !seen[it.Meta.DataShape] {
			seen[it.Meta.DataShape] = true
			shapes = append(shapes, it.Meta.DataShape)
		}
	}
	if len(shapes) < 2 {
		return ""
	}
	return fmt.Sprintf("[WARN] The data calibers of two consecutive calls are inconsistent (%s). Do NOT merge or compare them directly; first explicitly state the caliber difference.",
		strings.Join(shapes, " vs "))
}
