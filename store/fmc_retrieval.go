package store

import (
	"context"
	"fmt"
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
func (es *ExperienceStore) RetrieveRelevantExperience(
	ctx context.Context,
	query []float32,
	capability string,
	errorSignal string,
	tokenBudget int,
) []*ExperienceNode {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	var results []*ExperienceNode
	seen := make(map[string]bool)

	// Tier 1: Label-Precise Match (50% budget)
	predictedMode := es.predictFailureMode(errorSignal)

	tier1Limit := 5
	tier1Count := 0
	for _, node := range es.Nodes {
		if node.Outcome == "failure" && node.FailureMode != "" &&
			node.FailureMode == predictedMode && node.Capability == capability {
			if !seen[node.NodeID] {
				results = append(results, node)
				seen[node.NodeID] = true
				tier1Count++
				if tier1Count >= tier1Limit {
					break
				}
			}
		}
	}

	// Tier 2: Embedding Similarity (30% budget)
	if len(query) > 0 {
		simResults := es.querySimilarNodesLocked(query, 5)
		for _, node := range simResults {
			if !seen[node.NodeID] {
				results = append(results, node)
				seen[node.NodeID] = true
			}
		}
	}

	// Tier 3: AntiPatternStore keyword/tag match (ADDED 2026-09-08)
	// Surface historical pitfalls during assembly as ExperienceNodes.
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
	type scoredNode struct {
		node  *ExperienceNode
		score float64
	}
	var scored []scoredNode

	for _, node := range es.Nodes {
		if len(node.Embedding) > 0 {
			score := es.cosineSimilarity(query, node.Embedding)
			if score > 0.7 {
				scored = append(scored, scoredNode{node: node, score: score})
			}
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
