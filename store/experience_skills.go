package store

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	minSamplesToPrune = 20
	pruneWeightMargin = 0.15
)

// PruneSkills identifies underperforming skill variants (GeneratedSkills
// with a non-empty ParentID) that have accumulated enough samples to be
// judged fairly, and whose performance is meaningfully worse than the best
// sibling sharing the same capability. Pruned variants are moved into
// ArchivedSkills (never deleted outright -- their history stays queryable)
// and their IDs are returned so the caller can remove them from the live
// role/registry.Skills bindings.
//
// Root skills (ParentID == "") are never pruned, regardless of score --
// this is the reserve-a-minimum-selection-probability-for-the-baseline
// protection from the 2026-07-28 design: a root staying permanently in
// registry.Skills means it always remains a candidate in SelectSkill”s
// pool, so a temporarily-lucky new variant can never fully starve it out.
func (es *ExperienceStore) PruneSkills() []string {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	// Build per-capability top-2 ConfidenceDelta among skills with enough
	// samples, so the sibling lookup is O(1) per skill instead of O(N).
	// This turns PruneSkills from O(N²) into O(N).
	type scoreEntry struct {
		id    string
		delta float64
	}
	capBest := make(map[string]scoreEntry)   // highest delta per capability
	capSecond := make(map[string]scoreEntry) // second-highest delta per capability

	for id, skill := range es.GeneratedSkills {
		key := fmt.Sprintf("%s:%s", skill.Capability, id)
		sa, ok := es.SkillAffinities[key]
		if !ok || sa.SampleCount < minSamplesToPrune {
			continue
		}
		entry := scoreEntry{id: id, delta: sa.ConfidenceDelta}
		best, haveBest := capBest[skill.Capability]
		if !haveBest {
			capBest[skill.Capability] = entry
			continue
		}
		if entry.delta > best.delta {
			capBest[skill.Capability] = entry
			capSecond[skill.Capability] = best
		} else {
			second, haveSecond := capSecond[skill.Capability]
			if !haveSecond || entry.delta > second.delta {
				capSecond[skill.Capability] = entry
			}
		}
	}

	var pruned []string
	for id, skill := range es.GeneratedSkills {
		if skill.ParentID == "" {
			continue
		}
		key := fmt.Sprintf("%s:%s", skill.Capability, id)
		sa, ok := es.SkillAffinities[key]
		if !ok || sa.SampleCount < minSamplesToPrune {
			continue
		}

		// O(1) sibling lookup using pre-computed top-2.
		best, haveBest := capBest[skill.Capability]
		if !haveBest {
			continue
		}
		var bestSibling float64
		var haveSibling bool
		if best.id != id {
			bestSibling = best.delta
			haveSibling = true
		} else {
			second, haveSecond := capSecond[skill.Capability]
			if haveSecond {
				bestSibling = second.delta
				haveSibling = true
			}
		}
		if !haveSibling {
			continue
		}

		if sa.ConfidenceDelta < bestSibling-pruneWeightMargin {
			archived := *skill
			es.ArchivedSkills[id] = &archived
			delete(es.GeneratedSkills, id)
			pruned = append(pruned, id)
		}
	}
	return pruned
}

// bestSiblingScoreLocked returns the highest SkillAffinity.ConfidenceDelta
// among GeneratedSkills sharing the given capability (excluding excludeID),
// restricted to siblings with at least minSamples of their own -- so a
// brand-new, unproven sibling can never be used as the bar a variant is
// judged against. Caller must already hold es.Mu.
//
// Note: PruneSkills now uses an inline O(N) top-2 index instead of calling
// this O(N) helper per skill. This method is retained as a reference
// implementation and for potential use in other callers that need a
// single-skill sibling check.
func (es *ExperienceStore) bestSiblingScoreLocked(capability, excludeID string, minSamples int) (float64, bool) {
	best := math.Inf(-1)
	found := false
	for id, skill := range es.GeneratedSkills {
		if id == excludeID || skill.Capability != capability {
			continue
		}
		key := fmt.Sprintf("%s:%s", capability, id)
		sa, ok := es.SkillAffinities[key]
		if !ok || sa.SampleCount < minSamples {
			continue
		}
		if sa.ConfidenceDelta > best {
			best = sa.ConfidenceDelta
			found = true
		}
	}
	return best, found
}

// RootSkillForCapability returns the ID of the existing root GeneratedSkill
// (ParentID == "") for the given capability, or "" if none exists yet -- in
// which case the next skill registered for this capability should itself
// become the root (leave its own ParentID empty).
func (es *ExperienceStore) RootSkillForCapability(capability string) string {
	es.Mu.RLock()
	defer es.Mu.RUnlock()
	for id, skill := range es.GeneratedSkills {
		if skill.Capability == capability && skill.ParentID == "" {
			return id
		}
	}
	return ""
}

func (es *ExperienceStore) AddGeneratedSkill(ctx context.Context, skill *GeneratedSkill) error {
	es.Mu.Lock()
	defer es.Mu.Unlock()
	es.GeneratedSkills[skill.ID] = skill
	return nil
}

// QueryDecisionAdvice returns the historically most common resolved outcome
// for a given decisionType+roleID combination, or nil if there's no data.
//
// FIX (2026-07-08): this always returned nil because DecisionOutcomes was
// never populated in the first place (see the RecordTaskCompletion fix
// above); the query logic itself was also a stub. It now does a real
// majority-vote over resolved outcomes matching decisionType+roleID.
// QueryDecisionAdvice looks up the resolved decision outcome (majority vote)
// for a given decisionType+roleID pair. As of 2026-09-06, this has zero call
// sites in core/ — addDecision builds options from ODFTP triage and
// SelectSkill instead of consulting prior decision outcomes. It remains
// in the interface for a future "decision precedent injection" wiring.
func (es *ExperienceStore) QueryDecisionAdvice(ctx context.Context, decisionType, roleID string) *DecisionOutcome {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	choiceCounts := make(map[string]int)
	var confSum float64
	var confCount int
	var latestTS time.Time
	found := false

	for _, d := range es.DecisionOutcomes {
		if d.DecisionType != decisionType || d.RoleID != roleID || !d.Resolved {
			continue
		}
		found = true
		choiceCounts[d.Choice]++
		if d.FinalConfidence != nil {
			confSum += *d.FinalConfidence
			confCount++
		}
		if d.Timestamp.After(latestTS) {
			latestTS = d.Timestamp
		}
	}
	if !found {
		return nil
	}

	bestChoice := ""
	bestCount := -1
	for choice, count := range choiceCounts {
		if count > bestCount {
			bestChoice = choice
			bestCount = count
		}
	}

	result := &DecisionOutcome{
		DecisionType: decisionType,
		RoleID:       roleID,
		Choice:       bestChoice,
		Resolved:     true,
		Timestamp:    latestTS,
	}
	if confCount > 0 {
		avg := confSum / float64(confCount)
		result.FinalConfidence = &avg
	}
	return result
}

// QuerySimilarPatterns returns up to limit TaskPatterns whose capabilities
// overlap with the given list. Intended for use by the reflection engine
// or the Active Context Assembler to surface prior runs relevant to a new
// task — but as of 2026-09-06 it has zero call sites in core/ or tools/.
// It remains in the interface contract for future wiring.
func (es *ExperienceStore) QuerySimilarPatterns(ctx context.Context, capabilities []string, limit int) []TaskPattern {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	taskType := strings.Join(capabilities, "+")
	queryTokens := tokenize(taskType)
	var results []TaskPattern

	// Pre-calculate BM25 corpus stats
	N := len(es.TaskPatterns)
	if N == 0 {
		return nil
	}
	var totalLen float64
	termDocFreqs := make(map[string]int)
	patternTokensMap := make(map[string]map[string]int)

	for id, p := range es.TaskPatterns {
		tokens := tokenize(p.SourceText + " " + p.TaskType)
		patternTokensMap[id] = tokens
		docLen := 0
		for term, count := range tokens {
			docLen += count
			termDocFreqs[term]++
		}
		totalLen += float64(docLen)
	}
	avgdl := totalLen / float64(N)

	for id, p := range es.TaskPatterns {
		matchScore := 0.0

		// Phase 1: Vector Search (if available)
		// Vector search is not yet implemented; BM25 scoring below serves
		// as the production retrieval mechanism.

		// Phase 2: BM25 Scoring
		tokens := patternTokensMap[id]
		docLen := 0.0
		for _, count := range tokens {
			docLen += float64(count)
		}

		bm25Score := 0.0
		k1 := 1.2
		b := 0.75

		for term, qf := range queryTokens {
			df := termDocFreqs[term]
			if df == 0 {
				continue
			}
			idf := math.Log(1.0 + (float64(N)-float64(df)+0.5)/(float64(df)+0.5))
			f := float64(tokens[term])
			if f > 0 {
				termScore := idf * (f * (k1 + 1)) / (f + k1*(1-b+b*docLen/avgdl))
				bm25Score += termScore * float64(qf)
			}
		}

		// Normalize BM25 score to 0.0 - 1.0 range (heuristic)
		matchScore = math.Tanh(bm25Score / 5.0)

		if matchScore > 0.1 {
			p.MatchScore = matchScore
			results = append(results, p)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].MatchScore > results[j].MatchScore
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}
	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// QueryJITCandidates returns TaskPatterns that (a) textually resemble
// taskText and (b) have repeated the SAME fixed step composition
// (SequenceKey, not just similar wording -- see stepSequenceKey) often
// enough, with a high enough average confidence, to be worth suggesting as
// a candidate for compilation into a reusable JIT tool instead of
// re-planning the identical sequence from scratch every time.
//
// This depends on upsertTaskPattern's 2026-07-28 fix (SampleCount actually
// accumulating across repeated calls with the same SequenceKey) -- before
// that fix, SampleCount was always 1 and this function would never surface
// anything.
//
// Retrieval reuses QuerySimilarPatterns' existing BM25 text matching (broad
// recall), then applies a strict numeric filter (minSamples, minConfidence)
// on top -- the same two-stage "broad recall, then a real threshold" shape
// MatchSOPCandidates/FormatSOPHints already established for SOP hints.
// ADDED (2026-07-28).
func (es *ExperienceStore) QueryJITCandidates(ctx context.Context, taskText string, minSamples int, minConfidence float64, limit int) []TaskPattern {
	if taskText == "" || limit <= 0 {
		return nil
	}
	pool := es.QuerySimilarPatterns(ctx, []string{taskText}, limit*5)
	var out []TaskPattern
	for _, p := range pool {
		if p.SampleCount >= minSamples && p.AvgConfidence >= minConfidence {
			out = append(out, p)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// FormatJITCompositionHints renders a short, explicitly-non-mandatory
// reference block describing task patterns whose fixed role/capability/
// skill sequence has repeated often enough, with high enough confidence, to
// be worth considering for compilation into a reusable JIT tool
// (orchestrator_invoke(subsystem="jit", action="create", ...)) instead of
// re-planning the same sequence from scratch. Deliberately hedged, mirroring
// FormatSOPHints in core/sop_matcher.go: the LLM planning the task remains
// the decision-maker on whether a given pattern actually fits and whether
// compiling it is worthwhile. ADDED (2026-07-28).
func FormatJITCompositionHints(patterns []TaskPattern) string {
	if len(patterns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Reference: this task resembles a fixed sequence of role/capability/skill ")
	b.WriteString("steps that has been executed repeatedly before, with a consistently high success ")
	b.WriteString("rate. If the exact same sequence of tool calls would solve this task again, consider ")
	b.WriteString("compiling it into a reusable tool via orchestrator_invoke(subsystem=\"jit\", ")
	b.WriteString("action=\"create\", args={script, language, ...}) instead of re-planning it from ")
	b.WriteString("scratch. This is a suggestion, not a requirement -- use your own judgment about whether ")
	b.WriteString("this task is genuinely the same fixed composition, or only superficially similar.]\n")
	for _, p := range patterns {
		b.WriteString(fmt.Sprintf("- Pattern %s: seen %d times, avg confidence %.2f, task type %q\n",
			p.ID, p.SampleCount, p.AvgConfidence, p.TaskType))
	}
	return b.String()
}

// QuerySkillRecommendations returns generated/seed skill IDs bound to the
// given capability whose historical confidence effect (confidenceThreshold
// + average delta) meets or exceeds minConfidence, ranked best-first.
//
// FIX (2026-07-08): previously always returned nil, so this recommendation
// path was dead regardless of what data accumulated in SkillAffinities.
// Non-seed affinities require at least 2 samples before being trusted, to
// avoid recommending off a single noisy run; seed affinities (curated ahead
// of time) are trusted immediately.
func (es *ExperienceStore) QuerySkillRecommendations(ctx context.Context, capability string, minConfidence float64) []string {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	type scored struct {
		skill string
		delta float64
	}
	var candidates []scored
	for _, sa := range es.SkillAffinities {
		if sa.BaseCapability != capability {
			continue
		}
		if !sa.IsSeed && sa.SampleCount < 2 {
			continue
		}
		predicted := es.getConfidenceThreshold() + sa.ConfidenceDelta
		if predicted >= minConfidence {
			candidates = append(candidates, scored{skill: sa.AddedSkill, delta: sa.ConfidenceDelta})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].delta > candidates[j].delta })

	const maxRecommendations = 10
	if len(candidates) > maxRecommendations {
		candidates = candidates[:maxRecommendations]
	}
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.skill
	}
	return out
}

// tokenizeStopwords are common short function words that carry almost no
// distinguishing signal for keyword-overlap matching, but are short enough
// to slip past a pure length filter (e.g. "the"/"of" are length 2-3, same
// as many meaningful short words). Without filtering these, two completely
// unrelated task descriptions can appear to "overlap" purely because they
// both contain "the" or "of" -- this was caught by
// TestQueryRelevantSkills_OverlapAndNoFalsePositive during review, not
// discovered in production, but is exactly the kind of false-positive risk
// this function needs to guard against given how it's used (isNovelTask in
// core/reflection.go, skill auto-attach in core/spawner.go).
var tokenizeStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "and": true, "or": true, "are": true, "is": true,
	"was": true, "were": true, "be": true, "this": true, "that": true,
	"with": true, "as": true, "at": true, "by": true, "it": true, "if": true,
	"please": true, "me": true, "my": true, "your": true, "you": true,
}

// tokenize does a simple lowercase, alphanumeric-run tokenization, dropping
// very short/stopword tokens that tend to be noise (articles, particles,
// single CJK characters that don't carry much distinguishing signal on
// their own). It's intentionally simple: this is a keyword-overlap recall
// step, not a final relevance decision (same design principle used by
// core/sop_matcher.go's candidate recall -- see playbook 2026-07-08 notes).
func tokenize(s string) map[string]int {
	s = strings.ToLower(s)
	words := make(map[string]int)
	var b strings.Builder
	flush := func() {
		w := b.String()
		b.Reset()
		if len([]rune(w)) >= 2 && !tokenizeStopwords[w] {
			words[w]++
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return words
}

// QueryRelevantSkills returns generated skill IDs whose description or
// capability shares keyword overlap with taskType (which in practice is
// sometimes a canonical task-type string and sometimes a raw task
// description -- see core/spawner.go and core/reflection.go call sites),
// ranked by overlap count with historical success rate as a tiebreaker.
//
// FIX (2026-07-08): previously always returned nil. This had two concrete
// consequences that were silently broken:
//  1. core/reflection.go's isNovelTask() computed novelty as
//     len(QueryRelevantSkills(...)) == 0, which was therefore *always*
//     true -- every sufficiently confident, multi-step task was treated as
//     "novel" and crystallized into a new generated skill, regardless of
//     whether an equivalent skill already existed.
//  2. core/spawner.go's "Phase 0: Skill Retrieval" auto-attach of relevant
//     generated skills for roles with AllowDynamicSkills never fired, so
//     previously generated skills were written but never read back.
func (es *ExperienceStore) QueryRelevantSkills(ctx context.Context, taskType string, limit int) []string {
	// FIX (2026-08-16/17, playbook addendum): a nil-receiver guard, matching
	// the precedent set in core/resources.go's ResourceLoader.FetchCookbook
	// fix (07-24 addendum). core/spawner.go calls this whenever
	// role.AllowDynamicSkills is true, with no separate expStore!=nil check
	// at the call site -- several existing tests (and possibly some real
	// call paths) legitimately construct a Spawner with a nil expStore, so
	// without this guard any such role would crash the whole process.
	if es == nil {
		return nil
	}
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	if limit <= 0 || len(es.GeneratedSkills) == 0 {
		return nil
	}
	queryTokens := tokenize(taskType)
	if len(queryTokens) == 0 {
		return nil
	}

	type scored struct {
		id    string
		score float64
	}
	var candidates []scored
	for id, gs := range es.GeneratedSkills {
		// Environment Filter (ADDED 2026-08-16)
		if gs.OS != "" && gs.OS != runtime.GOOS {
			continue
		}

		haystack := tokenize(gs.Description + " " + gs.Capability)
		overlap := 0
		for t := range queryTokens {
			if count, ok := haystack[t]; ok && count > 0 {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		score := float64(overlap) + gs.SuccessRate*0.5
		candidates = append(candidates, scored{id: id, score: score})
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.id
	}
	return out
}

// QuerySkillsBySignal returns generated skill IDs that are mapped to a specific
// failure signal (e.g. an error message regex). ADDED (2026-08-16).
func (es *ExperienceStore) QuerySkillsBySignal(ctx context.Context, signal string) []string {
	// FIX (2026-08-16/17, playbook addendum): reproduced live via
	// TestSpawner_MultimodalFallbackDetection -- core/spawner.go's
	// retryable-error fallback path calls this unconditionally on
	// s.expStore with no nil check, and a nil expStore is a legitimate,
	// already-established test/construction pattern (see NewSpawner calls
	// with expStore=nil across several _test.go files). Without this guard
	// any provider call failure that reaches the fallback branch crashes
	// the whole orchestrator process, not just the one task.
	if es == nil {
		return nil
	}
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	if signal == "" || len(es.GeneratedSkills) == 0 {
		return nil
	}

	var results []string
	for id, gs := range es.GeneratedSkills {
		if gs.OS != "" && gs.OS != runtime.GOOS {
			continue
		}
		if gs.FailureSignal != "" && strings.Contains(strings.ToLower(signal), strings.ToLower(gs.FailureSignal)) {
			results = append(results, id)
		}
	}
	return results
}

// PruneByMetabolicROI scans all generated skills and applies metabolic pruning:
//   - Skills with metabolic_roi < 0.2 are marked "dormant" (removed from active
//     selection candidates but not deleted).
//   - Skills that have been "dormant" for > 60 days with no usage are physically
//     removed from GeneratedSkills and archived.
//
// Root skills (ParentID == "") are never pruned — same protection as PruneSkills.
// See docs/METABOLIC_PRUNING_DESIGN.md
func (es *ExperienceStore) PruneByMetabolicROI() []string {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	var pruned []string
	now := time.Now()
	dormancyThreshold := 0.2
	pruneAge := 60 * 24 * time.Hour

	for id, gs := range es.GeneratedSkills {
		if gs.ParentID == "" {
			continue
		}
		if gs.SkillStatus == "" {
			gs.SkillStatus = "active"
		}

		if gs.SkillStatus == "active" && gs.MetabolicROI > 0 && gs.MetabolicROI < dormancyThreshold {
			gs.SkillStatus = "dormant"
			continue
		}

		if gs.SkillStatus == "dormant" && now.Sub(gs.LastUsed) > pruneAge {
			archived := *gs
			es.ArchivedSkills[id] = &archived
			delete(es.GeneratedSkills, id)
			pruned = append(pruned, id)
		}
	}
	return pruned
}
