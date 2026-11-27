package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
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
// docs/completed/2026-09-15/PROMPT_GOVERNANCE_AND_BUDGET_DESIGN.md. It is applied
// as the final step of Build (the runtime two-level interception layer).
type PromptAssembler struct {
	expStore       store.IExperienceStore
	resourceLoader *ResourceLoader
	registry       *config.Registry
	logger         *obs.Logger
	budgetEnforcer *PromptBudgetEnforcer

	// archive (ADDED 2026-10-10): ContextArchive for Path D — querying
	// current-task step summaries to inject into the Warm layer.
	// Nil-safe: set via SetArchive from Spawner.
	// See docs/completed/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
	archive *ContextArchive

	// projectResolver resolves a taskID to its project space identifier.
	// Injected by Spawner via SetProjectResolver. Nil-safe: returns
	// "default" when nil (Phase 3, Project Space & Memory Tiering design).
	projectResolver func(taskID string) string

	// foldEngine (ADDED 2026-10-13): Segment-Based Incremental Fold engine.
	// Performs reversible compression of sealed segments before budget enforcement.
	// Nil-safe: when nil, fold is skipped (zero behavior change).
	// See docs/completed/SEGMENT_BASED_INCREMENTAL_FOLD_DESIGN.md.
	foldEngine *FoldEngine

	// memoryBankStore (ADDED 2026-10-17): injects UserProfile into sub-agent
	// prompts so spawned agents know user preferences/expertise.
	// Nil-safe: set via SetMemoryBankStore from Spawner.
	// See docs/completed/2026-10-17/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
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
	// See docs/completed/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §P2.
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

	// outputBase (ADDED 2026-10-30, D4 fix): the engine's output base
	// directory, used to construct the absolute task output path in
	// delegation mode prompts. Empty = not set (non-delegation path
	// or test); the SHARED-dir text omits the path suffix.
	// See DELEGATION_HANDOVER_REDESIGN.md §4.4.
	// §10.3 fix: guarded by outputBaseMu — SetOutputBase writes from the
	// spawner goroutine while Build reads. The value is constant across
	// calls (set once at engine init), so the race is memory-only; the
	// mutex prevents -race reports.
	outputBaseMu sync.Mutex
	outputBase   string
}

// SetArchive injects the ContextArchive for Path D (Regular Step Warm layer).
// Nil-safe: when nil, the Archive query is skipped (zero behavior change).
func (s *PromptAssembler) SetArchive(a *ContextArchive) {
	s.archive = a
}

// SetProjectResolver injects the taskID→projectID resolver.
// Nil-safe: when nil, ProjectOf returns "default".
func (s *PromptAssembler) SetProjectResolver(r func(taskID string) string) {
	s.projectResolver = r
}

// ProjectOf returns the project space identifier for the given task.
// Returns "default" when no resolver is wired.
func (s *PromptAssembler) ProjectOf(taskID string) string {
	if s.projectResolver == nil {
		return "default"
	}
	return s.projectResolver(taskID)
}

// SetOutputBase sets the engine's output base directory for D4 (delegation
// workspace path injection). Empty = skip path suffix in SHARED-dir text.
func (s *PromptAssembler) SetOutputBase(dir string) {
	s.outputBaseMu.Lock()
	s.outputBase = dir
	s.outputBaseMu.Unlock()
}

// SetFoldEngine injects the FoldEngine for segment-based incremental fold.
// Nil-safe: when nil, fold is skipped (zero behavior change).
func (s *PromptAssembler) SetFoldEngine(fe *FoldEngine) {
	s.foldEngine = fe
}

// SetMemoryBankStore injects the MemoryBankStore for UserProfile injection
// into sub-agent prompts. Nil-safe: when nil, no UserProfile is injected.
// ADDED 2026-10-17. See docs/completed/2026-10-17/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
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

func (s *PromptAssembler) collectCurrentSchemaFingerprints() map[string]string {
	if s.registry == nil {
		return nil
	}
	s.registry.Mu.RLock()
	defer s.registry.Mu.RUnlock()
	out := make(map[string]string, len(s.registry.MCPs)+len(s.registry.DynamicMCPs))
	for id, mcp := range s.registry.MCPs {
		if mcp.SchemaFingerprint != "" {
			out[id] = mcp.SchemaFingerprint
		}
	}
	for id, mcp := range s.registry.DynamicMCPs {
		if mcp.SchemaFingerprint != "" {
			out[id] = mcp.SchemaFingerprint
		}
	}
	return out
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
func NewPromptAssembler(expStore store.IExperienceStore, loader *ResourceLoader, reg *config.Registry, logger *obs.Logger) *PromptAssembler {
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
	stepID string, // D-PROMPT-4: for base_role.md template
) ([]schemas.ContentBlock, error) {
	// O14: keep Build as the sequential contract. Phase helpers preserve the
	// original query/side-effect order as well as the emitted block order.
	blocks := s.buildRoleAndMemoryBlocks(hub, role, precedents, taskID, stepID)

	// KV-2 (2026-10-27): volatile blocks (anti-pattern, similar-task,
	// active-context) are collected separately and appended AFTER the
	// constraints block (the cache marker). These blocks query the
	// experience store inside Build, and UpsertDecisionPrecedent writes
	// during the turn loop (spawner_dospawn.go:652) → they can change
	// mid-step. Placing them after the stable prefix keeps the prefix
	// byte-identical across turns → prefix cache hits.
	volatileBlocks := s.buildVolatileExperienceBlocks(ctx, hub, role, capability, toolConstraints, taskText, latestError)

	blocks = append(blocks, s.buildKnowledgeBlocks(ctx, hub, skillIDs, capability, providerCfg, fewShots, mergedContext, isolation)...)

	constraintsBlock, err := s.buildConstraintsBlock(role, capability, providerCfg, toolConstraints, mergedContext, taskID)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, constraintsBlock)

	// KV-2 (2026-10-27): append volatile blocks (anti-pattern, similar-task,
	// active-context) AFTER the cache marker so they don't poison the stable
	// prefix. These blocks query the experience store inside Build and can
	// change mid-step (UpsertDecisionPrecedent writes during the turn loop).
	blocks = append(blocks, volatileBlocks...)

	blocks = s.appendArchiveAndShapeBlocks(blocks, taskID)
	return s.finalizePromptBlocks(blocks, providerCfg, taskID), nil
}

// buildSkillPromptBlocks renders the skill section of the system prompt.
// Per §4.4, when multiple skills are loaded, use Progressive Disclosure
// (Trigger + one-line Description only). A single skill keeps its full
// prompt (no information loss when there's no contention).
func (s *PromptAssembler) buildSkillPromptBlocks(hub *ContextHub, skillIDs []string, providerCfg *config.ProviderConfig) []schemas.ContentBlock {
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
	if skillPart.Len() == 0 {
		return nil
	}
	// Skills are protected when single (full prompt), Tier 2 when
	// progressive-disclosed (already minimal, but tag for budget accounting).
	tier := GovTierProtected
	if len(loadedSkills) > 1 {
		tier = GovTier2
	}
	return []schemas.ContentBlock{TagBlockWithCategory(schemas.ContentBlock{
		Text: skillPart.String(),
	}, tier, CategorySkills)}
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

	currentFingerprints := s.collectCurrentSchemaFingerprints()
	var filtered []store.AntiPatternPrecedent
	var dropped int
	for _, p := range precedents {
		if schemas.PrecedentPassesSchemaGate(p.SchemaHashes, currentFingerprints) {
			filtered = append(filtered, p)
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		s.logger.Log("EventSchemaRotQuarantine", "", "", map[string]any{
			"source":             "prompt_assembler_antipattern",
			"precedents_dropped": dropped,
			"precedents_kept":    len(filtered),
		})
	}
	if len(filtered) == 0 {
		return ""
	}
	return formatAntiPatternWarnings(filtered)
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
// See docs/completed/STRUCTURED_TOOL_METADATA_DESIGN.md §2.3.
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
