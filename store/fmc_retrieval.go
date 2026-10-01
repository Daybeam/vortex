package store

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
)

// FailureModeProfile tracks the dominant failure modes for a specific model.
type FailureModeProfile struct {
	ModelID   string         `json:"model_id"`
	Provider  string         `json:"provider"`
	Counts    map[string]int `json:"counts"` // failure_mode → count
	UpdatedAt time.Time      `json:"updated_at"`
}

// RetrieveRelevantExperience does three-tier fusion retrieval.
// Tier 1: label-precise match on FailureMode + Capability
// Tier 2: embedding similarity fallback
// Tier 3: AntiPatternStore keyword/tag match (surfaced via active_context)
//
// When a rerankerClient is wired, coarse candidates are fine-reranked via
// System One and fused with RRF (whitepaper §3). Structured logging (slog)
// records per-tier counts, latency, and hit rates (whitepaper §2 / M2).
func (es *ExperienceStore) RetrieveRelevantExperience(
	ctx context.Context,
	query []float32,
	capability string,
	errorSignal string,
	tokenBudget int,
) []*ExperienceNode {
	es.Mu.RLock()

	log := slog.With("op", "RetrieveRelevantExperience", "capability", capability)
	overallStart := time.Now()

	var results []*ExperienceNode
	seen := make(map[string]bool)

	// Tier 1: Label-Precise Match (50% budget)
	// audit PERF-3: was O(N) full map scan. Now O(1) lookup via nodeIndex.
	tier1Start := time.Now()
	predictedMode := es.predictFailureMode(errorSignal)

	tier1Limit := 5
	if predictedMode != "" && es.nodeIndex != nil {
		key := nodeIndexKey(predictedMode, capability)
		for _, node := range es.nodeIndex[key] {
			if node.Outcome == "failure" && !seen[node.NodeID] {
				results = append(results, node)
				seen[node.NodeID] = true
				if len(results) >= tier1Limit {
					break
				}
			}
		}
	}

	// Hierarchical relaxation (design §2.4): when strict Tier 1 yields 0,
	// relax constraints — first by FailureMode only (any Capability), then
	// by Capability only (any FailureMode). This prevents over-fitting to
	// exact label matches and enables cross-capability experience reuse.
	tier1Relaxed := 0
	if len(results) == 0 && predictedMode != "" {
		for _, node := range es.Nodes {
			if node.Outcome == "failure" && node.FailureMode == predictedMode && !seen[node.NodeID] {
				results = append(results, node)
				seen[node.NodeID] = true
				tier1Relaxed++
				if tier1Relaxed >= tier1Limit {
					break
				}
			}
		}
	}
	if len(results) == 0 && capability != "" {
		for _, node := range es.Nodes {
			if node.Outcome == "failure" && node.Capability == capability && !seen[node.NodeID] {
				results = append(results, node)
				seen[node.NodeID] = true
				tier1Relaxed++
				if tier1Relaxed >= tier1Limit {
					break
				}
			}
		}
	}
	tier1Count := len(results)
	tier1Dur := time.Since(tier1Start)

	// Tier 2: Embedding Similarity (30% budget)
	tier2Start := time.Now()
	tier2Base := len(results)
	if len(query) > 0 {
		simResults := es.querySimilarNodesLocked(query, 5)
		for _, node := range simResults {
			if !seen[node.NodeID] {
				results = append(results, node)
				seen[node.NodeID] = true
			}
		}
	}
	tier2Count := len(results) - tier2Base
	tier2Dur := time.Since(tier2Start)

	// Tier 3: AntiPatternStore keyword/tag match (ADDED 2026-09-08)
	// Surface historical pitfalls during assembly as ExperienceNodes.
	tier3Start := time.Now()
	tier3Base := len(results)
	if len(results) < 15 {
		// Use error signal and capability to find relevant anti-patterns
		intent := predictedMode + " " + capability
		if errorSignal != "" {
			intent = errorSignal + " " + capability
		}

		aps := es.QueryRelevantAntiPatterns(intent, 3)
		for _, ap := range aps {
			if seen[ap.ID] {
				continue
			}
			// Map AntiPatternPrecedent to ExperienceNode for unified retrieval
			node := &ExperienceNode{
				NodeID:      ap.ID,
				Capability:  capability,
				FailureMode: ap.AntiPattern,
				Strategy:    ap.CorrectPattern,
				ErrorSignal: ap.Symptom,
				Outcome:     "failure",
				Critique:    fmt.Sprintf("ANTI-PATTERN: %s. CORRECT: %s", ap.AntiPattern, ap.CorrectPattern),
				Confidence:  ap.Confidence,
				SourceText:  ap.SourceText,
				Embedding:   ap.Embedding,
				Timestamp:   ap.LastSeen,
			}
			results = append(results, node)
			seen[ap.ID] = true
		}
	}
	tier3Count := len(results) - tier3Base
	tier3Dur := time.Since(tier3Start)

	// Context bonus scoring (design §2.3): environmental context matches add
	// incremental scoring weights rather than disqualifying patterns.
	// Re-sorts candidates by (tier rank + context bonus) before reranking.
	contextBonusDur := time.Duration(0)
	if len(results) > 1 && capability != "" {
		bonusStart := time.Now()
		results = applyContextBonus(results, capability)
		contextBonusDur = time.Since(bonusStart)
	}

	// audit P-2.3: release RLock before reranker HTTP call. The reranker
	// operates on the local results slice only (no es.Nodes access), so
	// holding the store lock during the network call blocked ALL experience
	// writes for the duration of the HTTP round-trip.
	es.Mu.RUnlock()

	// M3: System One Reranker + RRF Fusion (whitepaper §3).
	// Coarse ranking = results slice order (Tier1 → Tier2 → Tier3).
	// Fine ranking = reranker scores → converted to ranks → RRF fused.
	reranked := false
	rerankDur := time.Duration(0)
	if es.rerankerClient != nil && len(results) > 1 {
		rerankStart := time.Now()
		results, reranked = es.rerankWithRRF(ctx, log, results, errorSignal, capability)
		rerankDur = time.Since(rerankStart)
	}

	totalDur := time.Since(overallStart)
	log.Info("retrieval_complete",
		"total_candidates", len(results),
		"tier1_count", tier1Count,
		"tier1_relaxed", tier1Relaxed,
		"tier2_count", tier2Count,
		"tier3_count", tier3Count,
		"tier1_ms", tier1Dur.Milliseconds(),
		"tier2_ms", tier2Dur.Milliseconds(),
		"tier3_ms", tier3Dur.Milliseconds(),
		"context_bonus_ms", contextBonusDur.Milliseconds(),
		"rerank_ms", rerankDur.Milliseconds(),
		"reranked", reranked,
		"total_ms", totalDur.Milliseconds(),
	)

	return results
}

func (es *ExperienceStore) predictFailureMode(err string) string {
	if err == "" {
		return ""
	}
	lower := strings.ToLower(err)
	if strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") {
		return "rate_limit_hit"
	}
	if strings.Contains(lower, "401") || strings.Contains(lower, "403") || strings.Contains(lower, "permission") {
		return "auth_permission_denied"
	}
	if strings.Contains(lower, "timeout") {
		return "timeout_exceeded"
	}
	if strings.Contains(lower, "tool") && strings.Contains(lower, "not found") {
		return "tool_selection_error"
	}
	return ""
}

func (es *ExperienceStore) querySimilarNodesLocked(query []float32, limit int) []*ExperienceNode {
	if len(query) == 0 {
		return nil
	}
	type scoredNode struct {
		node  *ExperienceNode
		score float64
	}
	// audit P-1.3: preallocate slice + precompute query norm once.
	// Was: unsized slice + cosineSimilarity recomputed query norm per node.
	scored := make([]scoredNode, 0, len(es.Nodes))

	var queryNormSq float64
	for _, v := range query {
		queryNormSq += float64(v) * float64(v)
	}
	if queryNormSq == 0 {
		return nil
	}
	queryNorm := math.Sqrt(queryNormSq)

	for _, node := range es.Nodes {
		if len(node.Embedding) == 0 || len(node.Embedding) != len(query) {
			continue
		}
		var dot, normB float64
		for i := range query {
			dot += float64(query[i]) * float64(node.Embedding[i])
			normB += float64(node.Embedding[i]) * float64(node.Embedding[i])
		}
		if normB == 0 {
			continue
		}
		score := dot / (queryNorm * math.Sqrt(normB))
		if score > 0.7 {
			scored = append(scored, scoredNode{node: node, score: score})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}

	out := make([]*ExperienceNode, len(scored))
	for i, s := range scored {
		out[i] = s.node
	}
	return out
}

func (es *ExperienceStore) cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// applyContextBonus re-sorts retrieval candidates by combining tier rank with
// context-matching bonuses (design §2.3). Context dimensions are soft features
// — they add incremental weight rather than acting as binary filters.
//
// Base score = 1/(rank+1)  (inverse tier position, decays with rank)
// Capability match → +0.5 bonus
//
// This runs BEFORE the reranker so that RRF fusion combines the context-aware
// coarse ranking with the reranker's fine ranking.
func applyContextBonus(results []*ExperienceNode, capability string) []*ExperienceNode {
	type scored struct {
		node  *ExperienceNode
		score float64
	}
	scoredResults := make([]scored, len(results))
	for i, node := range results {
		score := 1.0 / float64(i+1) // base: inverse tier rank
		if node.Capability == capability {
			score += 0.5 // context bonus: capability match
		}
		scoredResults[i] = scored{node: node, score: score}
	}
	sort.Slice(scoredResults, func(i, j int) bool {
		return scoredResults[i].score > scoredResults[j].score
	})
	out := make([]*ExperienceNode, len(scoredResults))
	for i, s := range scoredResults {
		out[i] = s.node
	}
	return out
}

// GetFailureModeProfile returns the dominant failure modes for a specific model.
func (es *ExperienceStore) GetFailureModeProfile(modelID string) *FailureModeProfile {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	counts := make(map[string]int)
	provider := ""

	for _, node := range es.Nodes {
		if node.ModelID != modelID {
			continue
		}
		if node.Outcome == "failure" && node.FailureMode != "" {
			counts[node.FailureMode]++
		}
	}

	return &FailureModeProfile{
		ModelID:   modelID,
		Provider:  provider,
		Counts:    counts,
		UpdatedAt: time.Now(),
	}
}

// GetStatePotential returns the empirical success rate for a given state hash.
// ADDED (2026-09-08) for PGPO-based proactive intervention.
func (es *ExperienceStore) GetStatePotential(stateHash string) float64 {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	if sp, ok := es.StatePotentials[stateHash]; ok {
		return sp.Potential
	}
	// Default to 1.0 (optimistic) for unknown states to allow exploration.
	return 1.0
}

// nodeIndexKey builds the secondary index key for Tier 1 experience retrieval.
func nodeIndexKey(failureMode, capability string) string {
	return failureMode + "\x00" + capability
}

// rebuildNodeIndexLocked rebuilds the secondary index for O(1) Tier 1 lookup.
// Caller must hold es.Mu.Lock() (audit PERF-3: was O(N) scan per retrieval).
func (es *ExperienceStore) rebuildNodeIndexLocked() {
	es.nodeIndex = make(map[string][]*ExperienceNode)
	for _, node := range es.Nodes {
		if node.Outcome == "failure" && node.FailureMode != "" {
			key := nodeIndexKey(node.FailureMode, node.Capability)
			es.nodeIndex[key] = append(es.nodeIndex[key], node)
		}
	}
}

// RebuildNodeIndex rebuilds the secondary index for O(1) Tier 1 experience
// retrieval. Call this after directly mutating es.Nodes (e.g., in tests or
// batch imports) to keep the index in sync with the node map.
func (es *ExperienceStore) RebuildNodeIndex() {
	es.Mu.Lock()
	defer es.Mu.Unlock()
	es.rebuildNodeIndexLocked()
}

// ── System One Reranker + RRF Fusion (whitepaper §3) ──────────────────────

// rrfK is the Reciprocal Rank Fusion constant (whitepaper §3.2).
// k=60 is the standard value; higher k reduces the influence of top-ranked
// items, giving more weight to agreement between rankings.
const rrfK = 60

// rerankWithRRF applies System One fine reranking and RRF fusion to coarse
// retrieval results. Returns the reordered slice and true on success.
// On reranker error, returns the original order and false (graceful degradation).
// audit P-2.3: caller must NOT hold es.Mu — this function makes an HTTP call
// and only operates on the local results slice + es.rerankerClient (init-time).
func (es *ExperienceStore) rerankWithRRF(
	ctx context.Context,
	log *slog.Logger,
	results []*ExperienceNode,
	errorSignal string,
	capability string,
) ([]*ExperienceNode, bool) {
	// Build query text for the reranker.
	queryText := errorSignal
	if queryText == "" {
		queryText = capability
	}

	// Build document texts — prefer SourceText, fall back to other fields.
	docs := make([]string, len(results))
	for i, node := range results {
		text := node.SourceText
		if text == "" {
			text = node.Critique
		}
		if text == "" {
			text = node.Strategy
		}
		if text == "" {
			text = node.ErrorSignal
		}
		docs[i] = text
	}

	// Call reranker. Scores are runtime-only — never persisted (whitepaper §1.3).
	scores, err := es.rerankerClient.Rerank(ctx, queryText, docs)
	if err != nil {
		log.Warn("reranker_failed_using_coarse_order", "error", err)
		return results, false
	}
	if len(scores) != len(results) {
		log.Warn("reranker_score_count_mismatch",
			"expected", len(results), "got", len(scores))
		return results, false
	}

	// RRF fusion: combine coarse rank (slice order) with fine rank (by score).
	fusedOrder := rrfFuse(scores)

	reordered := make([]*ExperienceNode, len(results))
	for i, idx := range fusedOrder {
		reordered[i] = results[idx]
	}

	log.Debug("rerank_complete",
		"candidates", len(results),
		"rrf_k", rrfK,
	)

	return reordered, true
}

// rrfFuse computes Reciprocal Rank Fusion of two ranked lists:
//   - Coarse rank: candidate i has rank i (results slice order from Tier1→Tier2→Tier3)
//   - Fine rank:   candidates ranked by fineScores (higher score = better rank)
//
// RRF_Score(d) = 1/(k + coarseRank(d)) + 1/(k + fineRank(d))  (whitepaper §3.2)
//
// Returns indices in fused (best-first) order. Using rank — not absolute score —
// makes fusion immune to different scoring scales across backends (Jev vs Laya).
func rrfFuse(fineScores []float64) []int {
	n := len(fineScores)

	// Compute fine ranks: sort indices by score descending, rank 0 = best.
	fineOrder := make([]int, n)
	for i := range fineOrder {
		fineOrder[i] = i
	}
	sort.Slice(fineOrder, func(i, j int) bool {
		return fineScores[fineOrder[i]] > fineScores[fineOrder[j]]
	})
	fineRank := make([]int, n)
	for rank, idx := range fineOrder {
		fineRank[idx] = rank
	}

	// Compute RRF scores and sort by fused score descending.
	type rrfEntry struct {
		idx      int
		rrfScore float64
	}
	entries := make([]rrfEntry, n)
	for i := 0; i < n; i++ {
		coarseRank := i // results slice position = coarse rank
		entries[i] = rrfEntry{
			idx: i,
			rrfScore: 1.0/float64(rrfK+coarseRank) +
				1.0/float64(rrfK+fineRank[i]),
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].rrfScore > entries[j].rrfScore
	})

	out := make([]int, n)
	for i, e := range entries {
		out[i] = e.idx
	}
	return out
}
