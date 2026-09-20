package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ReflectionEngine handles the synthesis of execution data into reusable experience.
type ReflectionEngine struct {
	expStore  store.IExperienceStore
	taskStore store.ITaskStore
	registry  *config.Registry
	jit       *JITManager
	logger    *Logger
	wg        sync.WaitGroup // audit M7: track inner goroutines
}

// Wait blocks until all in-flight inner goroutines (hybrid merge,
// crystallization) finish. Called by DirectedEngine.Stop() after
// bgWg.Wait() so the process doesn't exit while reflection work
// is still in progress.
func (re *ReflectionEngine) Wait() {
	re.wg.Wait()
}

func NewReflectionEngine(
	es store.IExperienceStore,
	ts store.ITaskStore,
	reg *config.Registry,
	jit *JITManager,
	logger *Logger,
) *ReflectionEngine {
	return &ReflectionEngine{
		expStore:  es,
		taskStore: ts,
		registry:  reg,
		jit:       jit,
		logger:    logger,
	}
}

// ReflectOnTask is the entry point called after a task reaches terminal state.
// lifecycleCtx is the engine's lifecycle context — inner goroutines (hybrid
// merge, crystallization) derive their timeouts from it so they exit on
// shutdown instead of running for up to 5 minutes on a dead engine (audit C2).
func (re *ReflectionEngine) ReflectOnTask(lifecycleCtx context.Context, graph *schemas.TaskGraph) {
	if len(graph.Steps) == 0 {
		return
	}

	// Bound the entire reflection pass so background stores can't hang forever.
	// audit C3: derive from lifecycleCtx so reflection cancels on engine Stop()
	ctx, cancel := context.WithTimeout(lifecycleCtx, 30*time.Second)
	defer cancel()

	// 1. Extract step records
	records := make([]store.StepRecord, 0, len(graph.Steps))

	// Determine overall task type from completed steps
	var completedCaps []string
	for _, step := range graph.Steps {
		if step.Status != schemas.StepPending && step.Status != schemas.StepRunning {
			role := re.registry.Roles[step.RoleID]
			if role != nil {
				completedCaps = append(completedCaps, role.BaseCapability)
			}
		}
	}
	taskType := strings.Join(completedCaps, "+")
	if taskType == "" {
		taskType = "unknown"
	}

	for _, step := range graph.Steps {
		if step.Status == schemas.StepPending || step.Status == schemas.StepRunning {
			continue
		}

		// Resolve capability and skills via registry
		capability := "unknown"
		skills := []string{}
		if role, ok := re.registry.Roles[step.RoleID]; ok {
			capability = role.BaseCapability
			skills = append([]string{}, role.BoundSkills...)
		}
		skills = append(skills, step.AdditionalSkills...)

		conf := 0.0
		if step.Confidence != nil {
			conf = *step.Confidence
		}

		// Retrieve trace from TaskStore
		var trace []store.ToolInteraction
		if step.Status == schemas.StepOK {
			if res, _ := re.taskStore.Get(ctx, graph.TaskID, step.ID); res != nil {
				trace = res.Trace
			}
		}

		// ── JIT Auto-Promotion Capture (ADDED 2026-09-06) ────────────────
		// If this step's trace contains calls to any dynamically-generated
		// JIT tools (id prefix "jit_"), surface them as if they were bound
		// skills. The downstream maybePromoteSkill() hook (see reflection.go
		// around line 245) then runs its existing UsageCount/SuccessRate
		// UCB gate against them — no new promotion logic needed, we just
		// feed the JIT IDs into the same pipeline that already handles
		// role-bound skills. This closes the "capture" half of the JIT
		// lifecycle: agents that successfully escape a capability_required
		// via a runtime-generated tool now get their tool considered for
		// auto-promotion to the permanent registry after N successful uses.
		//
		// We also SEED the JIT ID into GeneratedSkills on first sighting so
		// that maybePromoteSkill's GetGeneratedSkill lookup can actually
		// find it — without seeding, an ephemeral JIT tool that never gets
		// promoted via registerAsSkill would be forever invisible to the
		// promotion gate.
		if step.Status == schemas.StepOK && len(trace) > 0 {
			seen := make(map[string]bool, len(skills))
			for _, sk := range skills {
				seen[sk] = true
			}
			for _, t := range trace {
				if !strings.HasPrefix(t.ToolName, "jit_") || seen[t.ToolName] {
					continue
				}
				seen[t.ToolName] = true
				skills = append(skills, t.ToolName)
				if _, exists := re.expStore.GetGeneratedSkill(t.ToolName); !exists {
					re.expStore.AddGeneratedSkill(ctx, &store.GeneratedSkill{
						ID:         t.ToolName,
						Capability: capability,
						// SuccessRate starts at 1.0: we only seed on StepOK.
						// UpdateRouteWeight in RecordTaskCompletion will
						// blend in subsequent failures on the affinity side,
						// and maybePromoteSkill's > 0.8 gate will naturally
						// filter out tools that start failing.
						SuccessRate: 1.0,
						LastUsed:    time.Now(),
						OS:          runtime.GOOS,
						Arch:        runtime.GOARCH,
						Shell:       env.GetShellVersion(),
					})
				} else {
					re.expStore.IncrementGeneratedSkillUsage(t.ToolName)
				}
			}
		}

		records = append(records, store.StepRecord{
			StepID:         step.ID,
			RoleID:         step.RoleID,
			ModelID:        step.ModelID,
			Skills:         skills,
			Capability:     capability,
			Confidence:     conf,
			Status:         string(step.Status),
			RetryCount:     step.RetryCount,
			MissingContext: step.MissingCtx,
			TaskType:       taskType,
			Task:           step.Task,
			Trace:          trace,
			LastError:      step.TriggerError,  // Pass the triggering signal (ADDED 2026-08-16)
			StatesVisited:  step.StatesVisited, // PGPO (ADDED 2026-09-08)
			Embedding:      step.Embedding,
			EmbeddingModel: step.EmbeddingModel,
		})
	}

	// 2. Reconstruct decisions
	decisions := make([]map[string]any, 0)
	for _, step := range graph.Steps {
		if step.Status == schemas.StepSkipped {
			decisions = append(decisions, map[string]any{
				"decision_type": schemas.DecisionStepFailed,
				"role_id":       step.RoleID,
				"choice":        "skip",
			})
		}

		// ACAIS: Detect environment failures for architectural awareness
		if step.Status == schemas.StepBlocked && strings.Contains(step.LastError, "missing dependency:") {
			parts := strings.Split(step.LastError, "missing dependency: ")
			if len(parts) > 1 {
				cmd := strings.TrimSpace(parts[1])
				re.expStore.RecordEnvironmentIssue(ctx, cmd, step.LastError)
			}
		}
	}

	overallConf := 0.0
	if graph.OverallConfidence != nil {
		overallConf = *graph.OverallConfidence
	}

	// FIX (2026-08-07, per Connor): light task-level credit assignment must
	// not penalize a skill/role for an infrastructure-caused failure it had
	// no control over (rate limits, auth/credential trouble, transient
	// network errors, a missing environment dependency). Scan every failing
	// step's LastError through the same classifyError taxonomy the
	// scheduler's own retry logic already uses (core/failure_classify.go) --
	// attributableFailure is true only if at least one failing step's error
	// does NOT fall into one of those infra-noise classes, i.e. there is a
	// positive signal the failure reflects on skill/role choice, not just an
	// absence of evidence either way. When ambiguous (no failing steps had a
	// LastError at all, or classification can't tell), this stays false --
	// under-penalizing on uncertain signal is the safer default.
	attributableFailure := false
	if graph.Status != schemas.GraphCompleted {
		for _, step := range graph.Steps {
			if step.LastError == "" {
				continue
			}
			switch classifyError(errors.New(step.LastError)) {
			case FailureClassRateLimit, FailureClassAuthPermission, FailureClassTransient, FailureClassMissingDependency:
				// infra/environment-caused -- not attributable to the skill/role
			default:
				attributableFailure = true
			}
		}
	}

	// 3. Commit to Experience Store
	experienceRecords := make([]store.StepRecord, 0, len(records))
	for _, rec := range records {
		if rec.Status == string(schemas.StepOK) || rec.Status == string(schemas.StepSkipped) {
			experienceRecords = append(experienceRecords, rec)
		}
	}
	re.expStore.RecordTaskCompletion(
		ctx,
		graph.TaskID,
		experienceRecords,
		overallConf,
		decisions,
		graph.Status == schemas.GraphCompleted,
		attributableFailure,
	)

	// Prune old/poor skills
	pruned := re.expStore.PruneSkills()
	if len(pruned) > 0 {
		var ops []config.PatchOp
		for _, id := range pruned {
			ops = append(ops, config.PatchOp{Op: config.OpRemove, Path: "/skills/" + id})
		}
		if err := re.registry.CommitOps(ops, "reflection_engine", ""); err != nil {
			fmt.Printf("[Reflection] Failed to prune skills via WAL: %v\n", err)
		}
	}

	// ── DarwinX Hybrid Merge (ADDED 2026-08-19) ──────────────────────────
	// Scan for complementary skill variants to merge their strengths.
	// Use a bounded context so the goroutine can't hang forever.
	// FIX (audit C2): derive from lifecycleCtx so shutdown cancels this.
	// FIX (audit M7): track with wg so Stop() waits for it.
	re.wg.Add(1)
	go func() {
		defer re.wg.Done()
		gctx, gcancel := context.WithTimeout(lifecycleCtx, 5*time.Minute)
		defer gcancel()
		re.ScanForHybridMerge(gctx)
		re.ScanAndPromoteCompoundSkills(gctx)
		re.expStore.PruneByMetabolicROI()
	}()

	// 4. Update Adaptive Routing Weights
	for _, rec := range records {
		success := rec.Status == string(schemas.StepOK)
		// We use confidence as the base score for the skill
		for _, skill := range rec.Skills {
			re.expStore.UpdateRouteWeight(ctx, rec.RoleID, rec.ModelID, skill, rec.Capability, rec.Confidence, success)

			// 4a. Check if we should promote a trial skill to permanent
			// Use context to potentially promote skill
			re.maybePromoteSkill(ctx, skill)
		}

		// 5. Crystallize Skill if successful, complex and NOVEL
		if success && len(rec.Trace) > 1 && rec.Confidence > 0.85 {
			if re.isNovelTask(ctx, rec.Task) {
				// Bounded context so crystallization can't hang forever.
				// FIX (audit C2): derive from lifecycleCtx so shutdown cancels this.
				// FIX (audit M7): track with wg so Stop() waits for it.
				re.wg.Add(1)
				go func(r store.StepRecord) {
					defer re.wg.Done()
					gctx, gcancel := context.WithTimeout(lifecycleCtx, 5*time.Minute)
					defer gcancel()
					re.CrystallizeStepToSkill(gctx, r)
				}(rec)
			}
		}
	}
}

func (re *ReflectionEngine) isNovelTask(ctx context.Context, task string) bool {
	// Simple keyword-based similarity check
	relevant := re.expStore.QueryRelevantSkills(ctx, task, 1)
	return len(relevant) == 0
}

func (re *ReflectionEngine) maybePromoteSkill(ctx context.Context, skillID string) {
	// FIX (2026-08-03): GetGeneratedSkill already acquires es.Mu.RLock()
	// internally. Wrapping it in an outer Lock()/Unlock() here was a
	// guaranteed self-deadlock: a goroutine holding a write Lock() can
	// never satisfy its own subsequent RLock() call, since Go's
	// sync.RWMutex is not reentrant. Confirmed live via a goroutine dump
	// showing three goroutines permanently blocked on es.Mu.RLock (one of
	// them this exact call site) after every task using a role with any
	// bound skill silently deadlocked the whole ExperienceStore.
	gs, ok := re.expStore.GetGeneratedSkill(skillID)

	if ok && !gs.IsVerified {
		// Criteria: Used 3+ times with > 80% success
		if gs.UsageCount >= 3 && gs.SuccessRate > 0.8 {
			err := re.jit.PromotePermanent(skillID)
			if err == nil {
				re.expStore.UpdateGeneratedSkillVerified(skillID, true)
				re.expStore.PersistAll(ctx)
			}
		}
	}
}

// CrystallizeStepToSkill abstracts a successful execution trace into a reusable tool.
func (re *ReflectionEngine) CrystallizeStepToSkill(ctx context.Context, rec store.StepRecord) {
	// Only proceed if we have a provider to call
	providerCfg := re.registry.GetProvider(re.registry.DefaultProviderName()) // audit H1: locked accessors
	if providerCfg == nil {
		return
	}
	provider, err := providers.Get(providerCfg, re.registry.ExternalRuntimes)
	if err != nil {
		return
	}

	traceBytes, _ := json.MarshalIndent(rec.Trace, "", "  ")

	// ── DarwinX Additive Editing (ADDED 2026-08-19) ──────────────────────
	// Check if a root skill already exists for this capability to enable delta editing
	parentID := re.expStore.RootSkillForCapability(rec.Capability)
	instruction := "Your task is to extract the logic from a successful execution trace and turn it into a reusable, parameterised Lua script."

	if parentID != "" {
		// Fetch existing code for additive editing
		instruction = fmt.Sprintf("A baseline Lua script already exists for capability %q (ID: %s). "+
			"Your task is to analyze the new execution trace and EXTEND the existing logic. "+
			"Do not rewrite from scratch unless necessary; perform an ADDITIVE EDIT that handles the new case while preserving old robustness.",
			rec.Capability, parentID)

		if baseCode, err := re.jit.GetSource(parentID); err == nil {
			instruction += fmt.Sprintf("\n\nExisting Baseline Code:\n```lua\n%s\n```", baseCode)
		}
	}

	system := fmt.Sprintf(`You are an expert automation engineer. %s
The script will be used as a standalone tool. It should handle errors gracefully and provide clear output.
Output ONLY the Lua code. No markdown, no explanation.`, instruction)

	user := fmt.Sprintf(`Task: %s
Capability: %s
Trace:
%s

Goal: Write a Lua script (or an extension of the existing one) that achieves this task.
Use variables for any hardcoded values that should be parameters.`, rec.Task, rec.Capability, string(traceBytes))

	resp, err := provider.Complete(ctx, providers.CompleteRequest{
		System:    system,
		User:      user,
		Model:     providerCfg.Model,
		MaxTokens: 2048,
		Secrets:   re.registry.Secrets,
	})
	if err != nil || resp.Text == "" {
		return
	}

	// Clean code
	code := resp.Text
	code = strings.TrimPrefix(code, "```lua")
	code = strings.TrimPrefix(code, "```")
	code = strings.TrimSuffix(code, "```")
	code = strings.TrimSpace(code)

	// Register as a temporary JIT tool with a long TTL (24h)
	id, err := re.jit.RegisterTool(code, "lua", 24*time.Hour, true)
	if err == nil {
		re.registerAsSkill(ctx, id, rec.Capability, rec.Task, rec.LastError)
	}
}

func (re *ReflectionEngine) registerAsSkill(ctx context.Context, id, capability, originalTask, failureSignal string) {
	// ── JIT Auto-Promotion Dedup (ADDED 2026-09-06) ─────────────────────
	// If the ID already exists in GeneratedSkills — which happens when a
	// JIT tool gets seeded by the trace-based capture hook in
	// RecordTaskCompletion — we MUST NOT overwrite it here. Crystallize-
	// StepToSkill re-generates a script and calls RegisterTool, which can
	// collide with an existing JIT ID (or worse, replace a healthy
	// success-rate history with a fresh SuccessRate=1.0 entry that
	// artificially boosts the maybePromoteSkill gate). Instead, just
	// nudge UsageCount and return.
	if gs, exists := re.expStore.GetGeneratedSkill(id); exists {
		if strings.HasPrefix(id, "jit_") {
			re.expStore.IncrementGeneratedSkillUsage(id)
			re.expStore.PersistAll(ctx)
			return
		}
		_ = gs // non-JIT duplicate path: fall through to existing logic below
	}
	// ── DarwinX Resampling Check (ADDED 2026-08-19) ──────────────────────
	// Instead of always creating a new random ID, check if a similar Candidate
	// (unverified) skill already exists for this capability to increment its count.
	existingCandidates := re.expStore.QueryRelevantSkills(ctx, originalTask, 1)
	if len(existingCandidates) > 0 {
		candID := existingCandidates[0]
		if gs, ok := re.expStore.GetGeneratedSkill(candID); ok && !gs.IsVerified && gs.Capability == capability {
			// Similar candidate found! Increment its usage and skip registration.
			re.expStore.IncrementGeneratedSkillUsage(candID)
			re.expStore.PersistAll(ctx)
			return
		}
	}

	// Lineage (ADDED 2026-08-01): if a root GeneratedSkill already exists
	// for this capability, the new one is registered as a variant sibling
	// (ParentID set to that root''s ID) instead of an unrelated independent
	// skill. If no root exists yet, this new skill becomes the root itself
	// (parentID left empty). See store.ExperienceStore.PruneSkills for how
	// underperforming variants eventually get archived.
	parentID := re.expStore.RootSkillForCapability(capability)

	// Priority 7: Use CommitOps for atomic increment with audit log
	newSkill := &config.Skill{
		ID:          id,
		Name:        "Generated: " + id,
		Capability:  capability,
		Description: "Autogenerated from successful task: " + originalTask,
		Implementations: map[string]config.SkillImplementation{
			"default": {
				SystemPrompt: fmt.Sprintf("You have access to a specialized tool `%s` that was generated to solve: %s. Use it when appropriate.", id, originalTask),
			},
		},
	}

	skillJSON, _ := json.Marshal(newSkill)
	ops := []config.PatchOp{
		{Op: config.OpAdd, Path: "/skills/" + id, Value: json.RawMessage(skillJSON)},
	}

	if err := re.registry.CommitOps(ops, "reflection_engine", ""); err != nil {
		fmt.Printf("[Reflection] Failed to persist JIT skill via WAL: %v\n", err)
	}

	// Record in ExperienceStore for retrieval
	re.expStore.AddGeneratedSkill(ctx, &store.GeneratedSkill{
		ID:            id,
		ParentID:      parentID,
		Description:   originalTask,
		Capability:    capability,
		UsageCount:    0,
		SuccessRate:   1.0,
		LastUsed:      time.Now(),
		OS:            runtime.GOOS,          // Environment Fingerprint (ADDED 2026-08-16)
		Arch:          runtime.GOARCH,        // Environment Fingerprint (ADDED 2026-08-16)
		Shell:         env.GetShellVersion(), // Environment Fingerprint (ADDED 2026-08-20)
		FailureSignal: failureSignal,         // Trigger-driven retrieval (ADDED 2026-08-16)
	})
}

// FormatOrchestrationBrief produces main agent-friendly text from experience data.
func (re *ReflectionEngine) ScanForHybridMerge(ctx context.Context) {
	// Group skills by capability
	byCap := make(map[string][]string)
	for id, gs := range re.expStore.GetGeneratedSkillsSnapshot() {
		if gs.IsVerified && gs.SuccessRate > 0.8 {
			byCap[gs.Capability] = append(byCap[gs.Capability], id)
		}
	}

	for cap, ids := range byCap {
		if len(ids) < 2 {
			continue
		}
		// If we have 2+ good variants, consider merging them
		re.logger.Log("EventHybridMergePlanned", "", "", map[string]any{
			"capability": cap,
			"variants":   ids,
			"message":    "DarwinX: Multiple successful variants detected. Merging complementary logic into a single expert skill.",
		})

		// Fetch source code for all variants
		sources := make(map[string]string)
		for _, id := range ids {
			if src, err := re.jit.GetSource(id); err == nil {
				sources[id] = src
			}
		}

		if len(sources) < 2 {
			continue
		}

		// Invoke LLM to unify them
		re.mergeVariants(ctx, cap, sources)
	}
}

func (re *ReflectionEngine) mergeVariants(ctx context.Context, capability string, sources map[string]string) {
	providerCfg := re.registry.GetProvider(re.registry.DefaultProviderName()) // audit H1: locked accessors
	if providerCfg == nil {
		return
	}
	provider, err := providers.Get(providerCfg, re.registry.ExternalRuntimes)
	if err != nil {
		return
	}

	var sourceList []string
	for id, src := range sources {
		sourceList = append(sourceList, fmt.Sprintf("-- Variant: %s\n%s", id, src))
	}

	system := `You are an expert Lua developer. Your task is to merge multiple Lua script variants into a single, unified, and robust Expert Skill.
The variants all target the same capability but might handle different edge cases or have different implementation strengths.
Unify them into a single script that is:
1. Robust: Handles all cases from all variants.
2. Clean: Removes redundancies.
3. Parameterized: Uses variables for values that might change.
Output ONLY the unified Lua code. No markdown, no explanation.`

	user := fmt.Sprintf("Capability: %s\n\nVariants to merge:\n\n%s", capability, strings.Join(sourceList, "\n\n------------------\n\n"))

	resp, err := provider.Complete(ctx, providers.CompleteRequest{
		System:    system,
		User:      user,
		Model:     providerCfg.Model,
		MaxTokens: 4096,
		Secrets:   re.registry.Secrets,
	})
	if err != nil || resp.Text == "" {
		return
	}

	// Clean code
	code := resp.Text
	code = strings.TrimPrefix(code, "```lua")
	code = strings.TrimPrefix(code, "```")
	code = strings.TrimSuffix(code, "```")
	code = strings.TrimSpace(code)

	// Register the unified tool
	id, err := re.jit.RegisterTool(code, "lua", 72*time.Hour, true) // Longer TTL for expert skills
	if err == nil {
		// Register as a NEW verified root skill
		re.registerAsSkill(ctx, id, capability, "Unified Expert Skill via Hybrid Merge", "")
		re.expStore.UpdateGeneratedSkillVerified(id, true)
		re.expStore.PersistAll(ctx)

		re.logger.Log("EventHybridMergeCompleted", "", id, map[string]any{
			"capability": capability,
			"message":    "DarwinX: Successfully unified variants into expert skill " + id,
		})
	}
}

func (re *ReflectionEngine) FormatOrchestrationBrief(ctx context.Context, capabilities []string) string {
	brief := re.expStore.GetOrchestrationBrief(ctx, capabilities)

	var lines []string
	lines = append(lines, "## Orchestration Experience Brief\n")

	// 1. Similar Patterns
	patterns, ok := brief["similar_patterns"].([]map[string]any)
	if ok && len(patterns) > 0 {
		lines = append(lines, "### Similar Past Task Patterns")
		for _, p := range patterns {
			taskType, _ := p["task_type"].(string)
			avgConf, _ := p["avg_confidence"].(float64)
			sampleCount, _ := p["sample_count"].(int)
			matchScore, _ := p["match_score"].(float64)

			lines = append(lines, fmt.Sprintf("- **%s** — avg confidence %.0f%%, %d runs, match score %.0f%%",
				taskType, avgConf*100, sampleCount, matchScore*100))

			seq, _ := p["step_sequence"].([]map[string]any)
			lines = append(lines, "  Suggested step sequence:")
			for _, s := range seq {
				roleID, _ := s["role_id"].(string)
				cap, _ := s["capability"].(string)
				skills, _ := s["skills"].([]string)
				skillsStr := strings.Join(skills, ", ")
				if skillsStr == "" {
					skillsStr = "base only"
				}
				lines = append(lines, fmt.Sprintf("    → role `%s` [%s] — skills: %s", roleID, cap, skillsStr))
			}
		}
		lines = append(lines, "")
	}

	// 2. Role Advice
	roleAdvice, ok := brief["role_advice"].(map[string]any)
	if ok && len(roleAdvice) > 0 {
		lines = append(lines, "### Role Performance History")
		for roleID, adviceAny := range roleAdvice {
			advice, ok := adviceAny.(map[string]any)
			if !ok || advice["experience"] == "none" {
				lines = append(lines, fmt.Sprintf("- `%s`: no history yet", roleID))
				continue
			}

			runs, _ := advice["total_runs"].(int)
			rate, _ := advice["success_rate"].(float64)
			avgConf, _ := advice["avg_confidence"].(float64)

			lines = append(lines, fmt.Sprintf("- `%s`: %d runs, success rate %.0f%%, avg confidence %.0f%%",
				roleID, runs, rate*100, avgConf*100))

			combos, _ := advice["recommended_skill_combos"].([][]string)
			if len(combos) > 0 {
				var comboStrs []string
				for i := 0; i < len(combos) && i < 2; i++ {
					comboStrs = append(comboStrs, strings.Join(combos[i], " + "))
				}
				lines = append(lines, fmt.Sprintf("  ✓ Best skill combos: %s", strings.Join(comboStrs, ", ")))
			}

			missing, _ := advice["common_missing_context"].([]string)
			if len(missing) > 0 {
				var missingStr []string
				for i := 0; i < len(missing) && i < 3; i++ {
					missingStr = append(missingStr, missing[i])
				}
				lines = append(lines, fmt.Sprintf("  ⚠ Often missing: %s", strings.Join(missingStr, ", ")))
			}
		}
		lines = append(lines, "")
	}

	// 3. Skill Recommendations
	skillRecs, ok := brief["skill_recommendations_by_capability"].(map[string]any)
	if ok && len(skillRecs) > 0 {
		lines = append(lines, "### Skill Recommendations by Capability")
		for cap, recsAny := range skillRecs {
			recs, ok := recsAny.([]map[string]any)
			if !ok || len(recs) == 0 {
				continue
			}
			var recStrs []string
			for _, r := range recs {
				sid, _ := r["skill_id"].(string)
				delta, _ := r["avg_confidence_improvement"].(float64)
				recStrs = append(recStrs, fmt.Sprintf("`%s` (+%.0f%%)", sid, delta*100))
			}
			lines = append(lines, fmt.Sprintf("- **%s**: consider adding %s", cap, strings.Join(recStrs, ", ")))
		}
		lines = append(lines, "")
	}

	// 4. Infrastructure Alerts
	infraAlerts, ok := brief["infrastructure_alerts"].(map[string]any)
	if ok && len(infraAlerts) > 0 {
		lines = append(lines, "### ⚠️ Infrastructure & Environment Alerts")
		for cmd, alertAny := range infraAlerts {
			alert, ok := alertAny.(map[string]any)
			if !ok {
				continue
			}
			msg, _ := alert["message"].(string)
			count, _ := alert["count"].(int)
			last, _ := alert["last_seen"].(time.Time)

			lines = append(lines, fmt.Sprintf("- **Command `%s`** — failed %d times. Last seen: %s",
				cmd, count, last.Format("2006-01-02 15:04")))
			lines = append(lines, fmt.Sprintf("  Error: %s", msg))
		}
		lines = append(lines, "")
	}

	if len(lines) <= 1 {
		return "_No experience data yet. Brief will populate after first task._"
	}

	return strings.Join(lines, "\n")
}

// isOutcomeDependent checks if toolA has divergent successors across runs.
// If toolA is followed by multiple different tools (with significant frequency),
// the sequence is outcome-dependent and should NOT be crystallized into a
// black-box compound skill — the agent needs to adapt based on intermediate results.
// See docs/architecture/OUTCOME_DEPENDENT_CRYSTALLIZATION_DESIGN.md
func (re *ReflectionEngine) isOutcomeDependent(ctx context.Context, toolA string) bool {
	partners, err := re.expStore.GetCooccurrencePartners(ctx, toolA, 3)
	if err != nil {
		return false // on error, don't block crystallization
	}
	// If toolA is followed by more than one distinct tool, it's outcome-dependent
	return len(partners) > 1
}

// ScanAndPromoteCompoundSkills scans the tool_cooccurrence table for high-frequency
// tool pairs and auto-crystallizes them into compound skills. A compound skill
// is a JSON file in workspace/skills/ that describes a proven tool sequence.
// Threshold: co_count >= 5 AND success_rate >= 0.85.
// ADDED (2026-09-13) — see docs/COMPOUND_SKILLS_DESIGN.md
// GUARDED (2026-09-15) — outcome-dependent pairs are skipped per OUTCOME_DEPENDENT_CRYSTALLIZATION_DESIGN.md
func (re *ReflectionEngine) ScanAndPromoteCompoundSkills(ctx context.Context) {
	candidates, err := re.expStore.GetCompoundCandidates(ctx, 5, 0.85)
	if err != nil {
		re.logger.Log("EventCompoundSkillError", "", "", map[string]any{"err": err.Error()})
		return
	}
	if len(candidates) == 0 {
		return
	}

	skillsDir := re.registry.SkillsDir
	if skillsDir == "" {
		return
	}

	for _, c := range candidates {
		// ── Outcome-Dependent Non-Crystallization Guard ──
		// If ToolA is followed by multiple different tools across runs,
		// the sequence is outcome-dependent — keep it decomposed so the
		// online controller can adapt based on intermediate results.
		if re.isOutcomeDependent(ctx, c.ToolA) {
			re.logger.Log("EventCompoundSkillSkippedOutcomeDependent", "", "", map[string]any{
				"tool_a":   c.ToolA,
				"tool_b":   c.ToolB,
				"co_count": c.CoCount,
			})
			continue
		}

		skillID := fmt.Sprintf("compound_%s_%s", c.ToolA, c.ToolB)

		if _, exists := re.registry.Skills[skillID]; exists {
			continue
		}

		skill := config.Skill{
			ID:          skillID,
			Name:        fmt.Sprintf("Compound: %s → %s", c.ToolA, c.ToolB),
			Capability:  "compound",
			Description: fmt.Sprintf("Auto-crystallized from %d co-occurrences (%.0f%% success rate). Executes %s then %s.", c.CoCount, c.SuccessRate()*100, c.ToolA, c.ToolB),
			Requires:    []string{c.ToolA, c.ToolB},
			InputSchema: map[string]any{
				"task": map[string]any{"type": "string", "description": "The compound task to execute"},
			},
			OutputSchema: map[string]any{
				"result": map[string]any{"type": "any"},
			},
		}

		skillJSON, err := json.MarshalIndent(skill, "", "  ")
		if err != nil {
			continue
		}

		skillPath := filepath.Join(skillsDir, skillID+".json")
		if err := os.WriteFile(skillPath, skillJSON, 0644); err != nil {
			re.logger.Log("EventCompoundSkillError", "", "", map[string]any{
				"skill_id": skillID,
				"err":      err.Error(),
			})
			continue
		}

		skill.InitFilter()
		re.registry.Skills[skillID] = &skill

		re.logger.Log("EventCompoundSkillPromoted", "", "", map[string]any{
			"skill_id":     skillID,
			"tool_a":       c.ToolA,
			"tool_b":       c.ToolB,
			"co_count":     c.CoCount,
			"success_rate": c.SuccessRate(),
		})
	}
}
