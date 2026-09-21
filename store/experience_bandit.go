package store

import (
	"context"
	"fmt"
	"math"
	"runtime"
)

func (es *ExperienceStore) SelectSkill(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string {
	if len(availableSkills) == 0 {
		return ""
	}
	explorationC := epsilon
	if explorationC <= 0 {
		explorationC = 0.1
	}

	es.Mu.RLock()
	defer es.Mu.RUnlock()

	type candidate struct {
		id      string
		score   float64
		samples int
	}
	cands := make([]candidate, 0, len(availableSkills))
	totalSamples := 0
	for _, sid := range availableSkills {
		score, samples := es.experienceScore(roleID, modelID, capability, sid)
		cands = append(cands, candidate{id: sid, score: score, samples: samples})
		totalSamples += samples
	}

	best := cands[0].id
	bestScore := math.Inf(-1)
	bestUnvisited := false
	for _, c := range cands {
		if c.samples == 0 {
			// Forced-exploration tier: any unvisited candidate outranks any
			// visited one, tie-broken among unvisited candidates by their
			// (possibly similarity-smoothed) prior score.
			if !bestUnvisited || c.score > bestScore {
				best = c.id
				bestScore = c.score
				bestUnvisited = true
			}
			continue
		}
		if bestUnvisited {
			continue
		}
		ucb := c.score + explorationC*math.Sqrt(math.Log(float64(totalSamples+1))/float64(c.samples))
		if ucb > bestScore {
			best = c.id
			bestScore = ucb
		}
	}
	return best
}

// capabilitySimilarity returns a token-overlap ratio in [0,1] between two
// capability descriptions, reusing this file's existing tokenize/
// tokenizeStopwords (originally built for QueryRelevantSkills -- see the
// 2026-07-08 addendum) rather than a new bespoke tokenizer.
func capabilitySimilarity(a, b string) float64 {
	ta := tokenize(a)
	tb := tokenize(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	overlap := 0
	for t := range ta {
		if _, ok := tb[t]; ok {
			overlap++
		}
	}
	smaller := len(ta)
	if len(tb) < smaller {
		smaller = len(tb)
	}
	return float64(overlap) / float64(smaller)
}

// similarityMinRatio / similaritySmoothingDamp (ADDED 2026-08-07): a
// candidate capability phrasing must overlap at least this fraction of the
// smaller token set with a prior recorded phrasing before that prior is
// borrowed as a cold-start smoothing signal; the borrowed score is then
// shrunk toward neutral (multiplied by the damp factor) since it was
// observed under different exact wording, not directly for this combo.
const (
	similarityMinRatio      = 0.5
	similaritySmoothingDamp = 0.5
)

// experienceScore looks up how well a given role+model+capability+skill
// combination has historically performed. It prefers the more specific
// RoutingMatrix entry (keyed by role and model) and falls back to the
// coarser SkillAffinity entry (keyed only by capability) when there's no
// routing history yet. Caller must hold es.Mu (read lock is sufficient).
//
// Returns (score, realSampleCount). realSampleCount is the number of
// directly-observed samples backing this exact (role,model,capability,
// skill) or (capability,skill) combo -- 0 means there is no direct
// experience with this exact combo, even if a non-zero score is returned
// (see the similarity-smoothing fallback below). Callers that need to
// distinguish "genuinely untried" from "tried and scored" -- e.g.
// SelectSkill's UCB1 forced-exploration tier -- must check realSampleCount,
// not just whether score is non-zero.
//
// FIX (2026-08-07): previously returned (0, false) for any capability
// phrasing that didn't exactly match a prior recorded key, even when this
// exact skillID had plenty of history under a near-identical but
// differently-worded capability (e.g. "fetch market data" vs "pull market
// quote data"). Every new phrasing therefore cold-started from scratch,
// discarding directly relevant prior experience. Now: if no exact key
// matches, look for the most similar recorded capability phrasing for the
// SAME skillID (via capabilitySimilarity, requiring at least
// similarityMinRatio overlap to avoid borrowing from a barely-related
// phrasing) and return its score damped toward neutral, with
// realSampleCount left at 0 so callers still correctly treat this exact
// combo as unexplored.
func (es *ExperienceStore) experienceScore(roleID, modelID, capability, skillID string) (float64, int) {
	// Environment Compatibility Check (ADDED 2026-08-16)
	if gs, ok := es.GeneratedSkills[skillID]; ok {
		if gs.OS != "" && gs.OS != runtime.GOOS {
			return -1.0, 0 // Strong negative for cross-platform mismatch
		}
	}

	if roleModels, ok := es.RoutingMatrix[roleID]; ok {
		if capMap, ok := roleModels[modelID][capability]; ok {
			if rw, ok := capMap[skillID]; ok && rw.TotalRuns > 0 {
				return rw.Weight, rw.TotalRuns
			}
		}
	}
	if sa, ok := es.SkillAffinities[fmt.Sprintf("%s:%s", capability, skillID)]; ok && sa.SampleCount > 0 {
		return sa.ConfidenceDelta, sa.SampleCount
	}

	// Similarity smoothing: scan SkillAffinities for this same skillID under
	// a different, textually-similar capability phrasing.
	bestSim := 0.0
	bestScore := 0.0
	found := false
	for _, sa := range es.SkillAffinities {
		if sa.AddedSkill != skillID || sa.SampleCount == 0 {
			continue
		}
		sim := capabilitySimilarity(capability, sa.BaseCapability)
		if sim >= similarityMinRatio && sim > bestSim {
			bestSim = sim
			bestScore = sa.ConfidenceDelta
			found = true
		}
	}
	// Also check RoutingMatrix across other capabilities for this same
	// role+model+skill, same similarity gate.
	if roleModels, ok := es.RoutingMatrix[roleID]; ok {
		if capMap, ok := roleModels[modelID]; ok {
			for capKey, skillMap := range capMap {
				rw, ok := skillMap[skillID]
				if !ok || rw.TotalRuns == 0 {
					continue
				}
				sim := capabilitySimilarity(capability, capKey)
				if sim >= similarityMinRatio && sim > bestSim {
					bestSim = sim
					bestScore = rw.Weight
					found = true
				}
			}
		}
	}
	if found {
		return bestScore * similaritySmoothingDamp, 0
	}
	return 0, 0
}

// minSamplesToPrune / pruneWeightMargin (ADDED 2026-08-01): a variant needs
// at least this many samples before its score is trusted for pruning
// decisions, and must score at least this much worse than the best
// comparably-sampled sibling before it is archived.
