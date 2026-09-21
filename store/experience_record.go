package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (es *ExperienceStore) RecordTaskCompletion(ctx context.Context, taskID string, records []StepRecord, overallConf float64, decisions []map[string]any, success bool, attributableFailure bool) error {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	taskType := deriveTaskType(records)
	es.upsertTaskPattern(taskID, taskType, records, overallConf)

	for _, rec := range records {
		es.updateRoleProfile(&rec)
		es.updateSkillAffinities(&rec)
		es.updateStatePotentialsLocked(ctx, &rec, success)
		es.updateSkillMetabolicCostLocked(&rec)
	}

	// ── Co-occurrence Tracking (ADDED 2026-09-13) ──────────────────────
	// Track consecutive capability pairs for Compound Skill crystallization.
	// See docs/COMPOUND_SKILLS_DESIGN.md
	es.trackCooccurrencesLocked(ctx, records)

	if !success && attributableFailure {
		es.applyTaskLevelPenalty(records)
	}

	// Update Role Affinity (EvoX - ADDED 2026-08-17)
	seenRoles := make(map[string]bool)
	for _, rec := range records {
		if seenRoles[rec.RoleID] {
			continue
		}
		seenRoles[rec.RoleID] = true
		es.updateRoleAffinityLocked(rec.RoleID, taskType, success)
	}

	// FIX (2026-07-08): decisions was accepted as a parameter but never
	// actually recorded anywhere, so DecisionOutcomes was permanently empty
	// and QueryDecisionAdvice could never return anything regardless of its
	// own implementation. Append a real outcome per reconstructed decision.
	for _, d := range decisions {
		dt, _ := d["decision_type"].(string)
		rid, _ := d["role_id"].(string)
		cap, _ := d["capability"].(string)
		choice, _ := d["choice"].(string)
		if dt == "" || choice == "" {
			continue
		}
		do := &DecisionOutcome{
			DecisionType: dt,
			RoleID:       rid,
			Capability:   cap,
			Choice:       choice,
			Resolved:     success,
			Timestamp:    time.Now(),
		}
		if success {
			oc := overallConf
			do.FinalConfidence = &oc
		}
		es.DecisionOutcomes = append(es.DecisionOutcomes, do)
	}
	if len(es.DecisionOutcomes) > es.getMaxDecisionOutcomes() {
		es.DecisionOutcomes = es.DecisionOutcomes[len(es.DecisionOutcomes)-es.getMaxDecisionOutcomes():]
	}

	// Debounce persist: if one is already in flight, skip — the next
	// persist that runs will pick up all accumulated changes. This
	// prevents unbounded goroutine spawn under burst task completion.
	if es.persistInFlight.CompareAndSwap(false, true) {
		go func() {
			defer es.persistInFlight.Store(false)
			es.PersistAll(ctx)
		}()
	}
	return nil
}

func (es *ExperienceStore) UpdateRouteWeight(ctx context.Context, roleID, modelID, skill, capability string, score float64, success bool) error {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	if es.RoutingMatrix[roleID] == nil {
		es.RoutingMatrix[roleID] = make(map[string]map[string]map[string]*RouteWeight)
	}
	if es.RoutingMatrix[roleID][modelID] == nil {
		es.RoutingMatrix[roleID][modelID] = make(map[string]map[string]*RouteWeight)
	}
	if es.RoutingMatrix[roleID][modelID][capability] == nil {
		es.RoutingMatrix[roleID][modelID][capability] = make(map[string]*RouteWeight)
	}

	rw := es.RoutingMatrix[roleID][modelID][capability][skill]
	if rw == nil {
		rw = &RouteWeight{Weight: 0.5}
		es.RoutingMatrix[roleID][modelID][capability][skill] = rw
	}
	rw.TotalRuns++
	if success {
		rw.SuccessCount++
	}
	rw.AvgScore = (rw.AvgScore*float64(rw.TotalRuns-1) + score) / float64(rw.TotalRuns)

	successRate := float64(rw.SuccessCount) / float64(rw.TotalRuns)
	if !success && !rw.LastUpdated.IsZero() {
		elapsed := time.Since(rw.LastUpdated)
		if elapsed > 1*time.Hour {
			decay := math.Exp(-elapsed.Hours() / 24.0)
			if decay < 0.5 {
				successRate = successRate*(1.0-decay) + 0.5*decay
			}
		}
	}
	rw.Weight = round2(0.7*successRate + 0.3*rw.AvgScore)
	rw.LastUpdated = time.Now()
	return nil
}

func (es *ExperienceStore) upsertTaskPattern(taskID, taskType string, records []StepRecord, overallConf float64) {
	if len(records) == 0 {
		return
	}

	steps := make([]map[string]any, len(records))
	sourceTexts := make([]string, 0, len(records))

	for i, r := range records {
		steps[i] = map[string]any{
			"role_id":    r.RoleID,
			"capability": r.Capability,
			"skills":     r.Skills,
		}
		if r.Task != "" {
			sourceTexts = append(sourceTexts, r.Task)
		}
	}

	avgTurns := 0.0
	if len(records) > 0 {
		totalTurns := 0
		for _, r := range records {
			totalTurns += len(r.Trace)
		}
		avgTurns = float64(totalTurns) / float64(len(records))
	}

	// Capture the first substantive task description as the pattern's source text
	fullSource := ""
	if len(sourceTexts) > 0 {
		fullSource = sourceTexts[0]
	}

	// FIX (2026-07-28): despite its name, this function never actually
	// looked for an existing matching pattern before this fix -- every call
	// created a brand-new TaskPattern with a fresh random ID and
	// SampleCount hardcoded to 1, so no pattern's SampleCount could ever
	// reflect genuine repetition of the same fixed step sequence. This made
	// any SampleCount-based threshold (e.g. "this exact composition has run
	// N times, consider compiling it into a JIT tool") meaningless, since N
	// could never exceed 1. Compute a canonical signature of the step
	// sequence (see stepSequenceKey) and merge into an existing pattern with
	// the same signature instead of always inserting a fresh one.
	key := stepSequenceKey(steps)
	if key != "" {
		for id, existing := range es.TaskPatterns {
			if existing.SequenceKey != key {
				continue
			}
			existing.SampleCount++
			existing.LastSeen = time.Now()
			n := float64(existing.SampleCount)
			existing.AvgConfidence = ((n-1)*existing.AvgConfidence + overallConf) / n
			existing.AvgTurnsUsed = ((n-1)*existing.AvgTurnsUsed + avgTurns) / n
			if fullSource != "" && existing.SourceText == "" {
				existing.SourceText = fullSource
			}
			es.TaskPatterns[id] = existing
			return
		}
	}

	patternID := "pat_" + uuid.New().String()[:8]
	es.TaskPatterns[patternID] = TaskPattern{
		ID:             patternID,
		TaskID:         taskID,
		TaskType:       taskType,
		StepSequence:   steps,
		SequenceKey:    key,
		SampleCount:    1,
		LastSeen:       time.Now(),
		MatchScore:     0.0,
		AvgConfidence:  overallConf,
		AvgTurnsUsed:   avgTurns,
		SourceText:     fullSource,
		Embedding:      records[0].Embedding,      // Capture from first step
		EmbeddingModel: records[0].EmbeddingModel, // Capture from first step
	}
}

// stepSequenceKey builds a canonical, order-preserving signature of a step
// sequence for exact-composition matching (role_id + capability + sorted
// skills per step, steps joined in original order). This is deliberately
// stricter than the BM25/text-similarity matching QuerySimilarPatterns uses
// for retrieval -- the JIT-composition-suggestion use case (see
// QueryJITCandidates) cares about "is this the literal same fixed tool
// composition", not "is this a similar-sounding task", so a cheap exact-key
// comparison is both correct and much faster than the BM25 scan. Returns ""
// for an empty step list, which upsertTaskPattern treats as "never merge"
// (matching the pre-fix behavior for that edge case).
func stepSequenceKey(steps []map[string]any) string {
	if len(steps) == 0 {
		return ""
	}
	parts := make([]string, len(steps))
	for i, s := range steps {
		roleID, _ := s["role_id"].(string)
		capability, _ := s["capability"].(string)
		var skillsStr string
		if sk, ok := s["skills"].([]string); ok && len(sk) > 0 {
			sorted := append([]string(nil), sk...)
			sort.Strings(sorted)
			skillsStr = strings.Join(sorted, ",")
		}
		parts[i] = roleID + "|" + capability + "|" + skillsStr
	}
	return strings.Join(parts, ">>")
}

func (es *ExperienceStore) updateRoleProfile(rec *StepRecord) {
	rp := es.RoleProfiles[rec.RoleID]
	if rp == nil {
		rp = &RoleProfile{RoleID: rec.RoleID}
		es.RoleProfiles[rec.RoleID] = rp
	}

	rp.TotalRuns++
	if rec.Status == "ok" {
		rp.SuccessCount++
	} else if rec.Status == "partial" {
		rp.PartialCount++
	} else {
		rp.FailureCount++
	}

	rp.AvgConfidence = (rp.AvgConfidence*float64(rp.TotalRuns-1) + rec.Confidence) / float64(rp.TotalRuns)
	rp.LastUpdated = time.Now()

	if len(rec.MissingContext) > 0 {
		rp.CommonMissingContext = append(rp.CommonMissingContext, rec.MissingContext...)
	}
}

func (es *ExperienceStore) updateSkillAffinities(rec *StepRecord) {
	for _, skill := range rec.Skills {
		key := fmt.Sprintf("%s:%s", rec.Capability, skill)
		sa := es.SkillAffinities[key]
		if sa == nil {
			sa = &SkillAffinity{
				BaseCapability: rec.Capability,
				AddedSkill:     skill,
			}
			es.SkillAffinities[key] = sa
		}
		sa.SampleCount++
		delta := rec.Confidence - es.getConfidenceThreshold()
		sa.ConfidenceDelta = (sa.ConfidenceDelta*float64(sa.SampleCount-1) + delta) / float64(sa.SampleCount)
		sa.LastUpdated = time.Now()
	}
}

func (es *ExperienceStore) updateStatePotentialsLocked(ctx context.Context, rec *StepRecord, success bool) {
	for _, stateHash := range rec.StatesVisited {
		sp := es.StatePotentials[stateHash]
		if sp == nil {
			// Extract tool name from state hash if possible, or leave empty
			// StateHash format: ToolID:ObservationHash
			toolID := ""
			if parts := strings.Split(stateHash, ":"); len(parts) > 0 {
				toolID = parts[0]
			}
			sp = &StatePotential{
				StateHash: stateHash,
				ToolID:    toolID,
			}
			es.StatePotentials[stateHash] = sp
		}
		sp.TotalRuns++
		if success {
			sp.SuccessCount++
		}
		sp.Potential = float64(sp.SuccessCount) / float64(sp.TotalRuns)
		sp.LastUpdated = time.Now()

		if es.backend != nil {
			// audit H10: track with saveWg so shutdown waits for this;
			// use caller's ctx so save cancels on shutdown.
			es.saveWg.Add(1)
			go func(ctx context.Context, sp *StatePotential) {
				defer es.saveWg.Done()
				es.backend.SaveStatePotential(ctx, sp)
			}(ctx, sp)
		}
	}
}

// trackCooccurrencesLocked extracts consecutive capability pairs from step
// records and upserts them into the tool_cooccurrence table. Caller MUST hold es.Mu.
// ADDED (2026-09-13) — see docs/COMPOUND_SKILLS_DESIGN.md
func (es *ExperienceStore) trackCooccurrencesLocked(ctx context.Context, records []StepRecord) {
	for i := 0; i < len(records)-1; i++ {
		toolA := records[i].Capability
		toolB := records[i+1].Capability
		if toolA == "" || toolB == "" || toolA == toolB {
			continue
		}
		success := records[i+1].Status == "ok"
		entry := &CooccurrenceEntry{
			ToolA:        toolA,
			ToolB:        toolB,
			CoCount:      1,
			SuccessCount: 0,
			LastUpdated:  time.Now(),
		}
		if success {
			entry.SuccessCount = 1
		}
		if es.backend != nil {
			// audit H10: track with saveWg so shutdown waits for this;
			// use caller's ctx so save cancels on shutdown.
			es.saveWg.Add(1)
			go func(ctx context.Context, entry *CooccurrenceEntry) {
				defer es.saveWg.Done()
				es.backend.SaveCooccurrence(ctx, entry)
			}(ctx, entry)
		}
	}
}

// GetCompoundCandidates queries the backend for tool pairs that meet the
// Compound Skill promotion threshold. Returns empty list if backend doesn't
// support co-occurrence queries.
func (es *ExperienceStore) GetCompoundCandidates(ctx context.Context, minCount int, minSuccessRate float64) ([]*CooccurrenceEntry, error) {
	if es.backend == nil {
		return nil, nil
	}
	return es.backend.GetCompoundCandidates(ctx, minCount, minSuccessRate)
}

// GetCooccurrencePartners returns all tools that follow toolA in execution
// traces with count >= minCount. Used to detect outcome-dependent branching.
func (es *ExperienceStore) GetCooccurrencePartners(ctx context.Context, toolA string, minCount int) ([]*CooccurrenceEntry, error) {
	if es.backend == nil {
		return nil, nil
	}
	return es.backend.GetCooccurrencePartners(ctx, toolA, minCount)
}

// lightFailurePenaltyDelta is the synthetic confidence delta folded into a
// skill's running average when the overall task it was used in failed for
// a reason attributable to skill/role choice (see applyTaskLevelPenalty).
// Deliberately smaller in magnitude than a genuinely bad per-step
// Confidence would produce -- this is a small SUPPLEMENTARY signal on top
// of each step's own already-recorded local outcome (updateSkillAffinities
// above), not a replacement for it, so a step already judged on its own
// merits is never double-punished as catastrophic just because the task
// eventually failed for unrelated downstream reasons.
const lightFailurePenaltyDelta = -0.05

// applyTaskLevelPenalty (ADDED 2026-08-07, per Connor -- light credit
// assignment across a task's trajectory, explicitly scoped down from full
// RL-style return propagation) nudges every skill used anywhere in this
// task's records with one additional small negative sample. Caller
// (RecordTaskCompletion) only invokes this when the task failed AND the
// failure was classified as attributable to skill/role choice quality --
// never for rate-limiting, auth/credential trouble, a missing environment
// dependency, or other infra-level noise the skill had no way to avoid.
// Each step already gets judged on its own local Confidence via
// updateSkillAffinities/updateRoleProfile; this is a small supplementary
// nudge, not a substitute, and is skipped entirely (by the caller, via
// attributableFailure) rather than guessed at when the failure cause is
// ambiguous.
func (es *ExperienceStore) applyTaskLevelPenalty(records []StepRecord) {
	seen := make(map[string]bool)
	for _, rec := range records {
		for _, skill := range rec.Skills {
			key := fmt.Sprintf("%s:%s", rec.Capability, skill)
			if seen[key] {
				continue // don't nudge the same skill twice for one task
			}
			seen[key] = true
			sa := es.SkillAffinities[key]
			if sa == nil {
				sa = &SkillAffinity{BaseCapability: rec.Capability, AddedSkill: skill}
				es.SkillAffinities[key] = sa
			}
			sa.SampleCount++
			sa.ConfidenceDelta = (sa.ConfidenceDelta*float64(sa.SampleCount-1) + lightFailurePenaltyDelta) / float64(sa.SampleCount)
			sa.LastUpdated = time.Now()
		}
	}
}

func (es *ExperienceStore) GetGeneratedSkill(id string) (*GeneratedSkill, bool) {
	es.Mu.RLock()
	defer es.Mu.RUnlock()
	gs, ok := es.GeneratedSkills[id]
	return gs, ok
}

func (es *ExperienceStore) GetGeneratedSkillsSnapshot() map[string]*GeneratedSkill {
	es.Mu.RLock()
	defer es.Mu.RUnlock()
	snapshot := make(map[string]*GeneratedSkill, len(es.GeneratedSkills))
	for k, v := range es.GeneratedSkills {
		snapshot[k] = v
	}
	return snapshot
}

func (es *ExperienceStore) Lock() {
	es.Mu.Lock()
}

func (es *ExperienceStore) Unlock() {
	es.Mu.Unlock()
}

func (es *ExperienceStore) UpdateGeneratedSkillVerified(id string, verified bool) {
	es.Mu.Lock()
	defer es.Mu.Unlock()
	if gs, ok := es.GeneratedSkills[id]; ok {
		gs.IsVerified = verified
	}
}

// IncrementGeneratedSkillUsage atomically increments the usage count and
// updates the last-used timestamp for a generated skill.
func (es *ExperienceStore) IncrementGeneratedSkillUsage(id string) {
	es.Mu.Lock()
	defer es.Mu.Unlock()
	if gs, ok := es.GeneratedSkills[id]; ok {
		gs.UsageCount++
		gs.LastUsed = time.Now()
	}
}

func (es *ExperienceStore) QueryRoleAdvice(ctx context.Context, roleID string) map[string]any {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	res := make(map[string]any)
	if rp, ok := es.RoleProfiles[roleID]; ok {
		res["recommended_skill_combos"] = rp.HighConfidenceSkillCombos
	}
	return res
}

// SelectSkill chooses among availableSkills using UCB1 (upper confidence
// bound), preferring skills with strong historical scores while still
// systematically exploring under-tried options -- more sample-efficient
// than flat epsilon-greedy random exploration, and, crucially,
// deterministic (no rand call anywhere in this function).
//
// FIX (2026-08-07): the previous version (2026-07-08 fix) was a real
// epsilon-greedy implementation, but flat-epsilon exploration has two
// well-known weaknesses this replaces: (1) it explores at the same fixed
// rate forever, wasting picks on random choices even once an option has
// accumulated enough samples to be confidently ranked; (2) a brand-new
// candidate with zero samples gets no special treatment -- it's just
// however experienceScore's cold-start default (0, 0 samples) happens to
// rank against already-scored options, so a genuinely untried option could
// easily never get picked at all if anything else has ever scored above
// zero. UCB1 fixes both: any zero-sample candidate is *always* preferred
// over any sampled candidate (forced exploration of every option at least
// once, same principle as "try every arm once" in classic bandit theory,
// tie-broken by cold-start similarity-smoothed prior score -- see
// experienceScore below -- rather than arbitrarily), and for candidates
// that do have samples, the exploration bonus
// (explorationC * sqrt(ln(totalSamples)/thisCandidateSamples)) shrinks
// automatically as a candidate accumulates more samples, so confident,
// well-established options stop being second-guessed by random chance
// while options with only a few samples keep getting a fair second look.
//
// The epsilon parameter name is kept for call-site compatibility (see
// core/scheduler.go) but is now interpreted as UCB1's exploration
// coefficient C, not a random-pick probability -- 0.1 (the existing call
// site's value) is a reasonable, fairly conservative C for a live
// production system where a wrong exploratory pick has a real cost, versus
// a simulated bandit benchmark where C~1-2 is more typical.

// updateSkillMetabolicCostLocked updates token cost, latency, and ROI for
// each skill used in a step. Caller must hold es.Mu.
// ROI = (SuccessRate * 100) / max(Cost, 0.1)
// Cost = w1*(Tokens/1000) + w2*(Latency/1000) + w3*RetryCount
// See docs/METABOLIC_PRUNING_DESIGN.md
func (es *ExperienceStore) updateSkillMetabolicCostLocked(rec *StepRecord) {
	success := rec.Status == "ok"
	for _, skillID := range rec.Skills {
		gs, ok := es.GeneratedSkills[skillID]
		if !ok {
			continue
		}
		gs.TokenCostTotal += rec.TokenUsed
		if rec.LatencyMs > 0 {
			if gs.UsageCount == 0 {
				gs.AvgLatencyMs = rec.LatencyMs
			} else {
				gs.AvgLatencyMs = (gs.AvgLatencyMs*float64(gs.UsageCount) + rec.LatencyMs) / float64(gs.UsageCount+1)
			}
		}
		gs.UsageCount++
		gs.LastUsed = time.Now()
		if gs.UsageCount > 0 {
			if success {
				gs.SuccessRate = (gs.SuccessRate*float64(gs.UsageCount-1) + 1.0) / float64(gs.UsageCount)
			} else {
				gs.SuccessRate = (gs.SuccessRate * float64(gs.UsageCount-1)) / float64(gs.UsageCount)
			}
		}
		cost := 0.3*float64(gs.TokenCostTotal)/1000.0 + 0.3*gs.AvgLatencyMs/1000.0 + 0.4*float64(rec.RetryCount)
		if cost < 0.1 {
			cost = 0.1
		}
		gs.MetabolicROI = (gs.SuccessRate * 100.0) / cost
		if gs.SkillStatus == "" {
			gs.SkillStatus = "active"
		}
	}
}
