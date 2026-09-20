package store

import (
	"context"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
	"log"
	"sort"
	"strings"
	"time"
)

func (es *ExperienceStore) GetOrchestrationBrief(ctx context.Context, skillIDs []string) map[string]any {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	res := make(map[string]any)
	patterns := es.QuerySimilarPatterns(ctx, skillIDs, 3)

	// LlamaIndex-inspired Recursive Retrieval:
	// If a pattern references a previous TaskID, load its execution details to enrich context.
	enrichedPatterns := make([]map[string]any, 0)
	for _, p := range patterns {
		pDict := map[string]any{
			"pattern_id":     p.ID,
			"task_id":        p.TaskID,
			"task_type":      p.TaskType,
			"step_sequence":  p.StepSequence,
			"sample_count":   p.SampleCount,
			"avg_confidence": p.AvgConfidence,
			"match_score":    p.MatchScore,
		}
		if p.TaskID != "" && es.taskStore != nil {
			pDict["has_linked_context"] = true
			pDict["source_task_id"] = p.TaskID
		}
		enrichedPatterns = append(enrichedPatterns, pDict)
	}

	res["similar_patterns"] = enrichedPatterns

	// Populate Role Advice
	roleAdvice := make(map[string]any)
	for _, rp := range es.RoleProfiles {
		if rp.TotalRuns < 2 {
			continue
		}
		advice := map[string]any{
			"total_runs":               rp.TotalRuns,
			"success_rate":             float64(rp.SuccessCount) / float64(rp.TotalRuns),
			"avg_confidence":           rp.AvgConfidence,
			"recommended_skill_combos": rp.HighConfidenceSkillCombos,
			"common_missing_context":   rp.CommonMissingContext,
		}
		roleAdvice[rp.RoleID] = advice
	}
	res["role_advice"] = roleAdvice

	// Populate Skill Recommendations by Capability
	skillRecs := make(map[string]any)
	capabilities := make(map[string]bool)
	for _, p := range patterns {
		for _, step := range p.StepSequence {
			if cap, ok := step["capability"].(string); ok {
				capabilities[cap] = true
			}
		}
	}
	for _, sid := range skillIDs {
		capabilities[sid] = true // treat skillIDs as capability seeds if provided
	}

	for cap := range capabilities {
		recs := es.QuerySkillRecommendations(ctx, cap, 0.8)
		if len(recs) > 0 {
			skillRecs[cap] = recs
		}
	}
	res["skill_recommendations_by_capability"] = skillRecs

	alerts := es.GetEnvironmentAlerts(ctx)
	if len(alerts) > 0 {
		res["infrastructure_alerts"] = alerts
	}

	return res
}

func (es *ExperienceStore) RecordEnvironmentIssue(ctx context.Context, cmd string, message string) error {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	issue, ok := es.EnvironmentIssues[cmd]
	if !ok {
		issue = &EnvironmentIssue{
			Command: cmd,
		}
		es.EnvironmentIssues[cmd] = issue
	}
	issue.Message = message
	issue.Count++
	issue.LastSeen = time.Now()
	issue.IsResolved = false

	return nil
}

// Reindex re-embeds all TaskPatterns using the given provider+model.
// Intended for bulk re-embedding when the embedding model changes, but
// as of 2026-09-06 has zero call sites in core/ or tools/. The function
// was previously exposed via admin.reindex_memory (now consolidated
// into orchestrator_invoke). It remains in the interface for future
// admin re-embedding tool wiring.
func (es *ExperienceStore) Reindex(ctx context.Context, provider interfaces.Provider, modelID string) error {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	total := len(es.TaskPatterns)
	count := 0

	for id, p := range es.TaskPatterns {
		// Only re-index if model changed OR embedding missing
		if p.EmbeddingModel == modelID && len(p.Embedding) > 0 {
			continue
		}

		text := p.SourceText
		if text == "" {
			// Fallback: use task type as source text if original is lost
			text = p.TaskType
		}

		emb, err := provider.Embed(ctx, text)
		if err != nil {
			log.Printf("[ExperienceStore] Failed to re-index pattern %s: %v", id, err)
			continue
		}

		p.Embedding = emb
		p.EmbeddingModel = modelID
		es.TaskPatterns[id] = p
		count++

		// Self-healing: Ensure SourceText is preserved for future migrations
		if p.SourceText == "" {
			p.SourceText = text
			es.TaskPatterns[id] = p
		}
	}

	log.Printf("[ExperienceStore] Re-indexing complete. Migrated %d/%d patterns to %s", count, total, modelID)
	return es.PersistAll(ctx)
}

func (es *ExperienceStore) updateRoleAffinityLocked(roleID, taskType string, success bool) {
	if es.RoleAffinities == nil {
		es.RoleAffinities = make(map[string]map[string]float64)
	}
	if es.RoleAffinities[roleID] == nil {
		es.RoleAffinities[roleID] = make(map[string]float64)
	}

	current, ok := es.RoleAffinities[roleID][taskType]
	if !ok {
		current = 0.5 // Start with neutral affinity
	}

	if success {
		// Increment affinity
		current += 0.05
		if current > 1.0 {
			current = 1.0
		}
	} else {
		// Decrement affinity
		current -= 0.02
		if current < 0 {
			current = 0
		}
	}
	es.RoleAffinities[roleID][taskType] = round2(current)
}

func (es *ExperienceStore) QueryRoleAffinity(ctx context.Context, roleID, taskType string) float64 {
	// Defense-in-depth nil-receiver guard (playbook addendum, consistent with
	// QueryRelevantSkills/QuerySkillsBySignal). Neutral default matches the
	// existing "no data yet" return value below.
	if es == nil {
		return 0.5
	}
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	if es.RoleAffinities == nil || es.RoleAffinities[roleID] == nil {
		return 0.5 // Neutral default
	}
	score, ok := es.RoleAffinities[roleID][taskType]
	if !ok {
		return 0.5
	}
	return score
}

// GetEnvironmentAlerts returns unresolved environment issues seen in the
// last 24 hours. Intended for the health dashboard / admin telemetry, but
// as of 2026-09-06 has zero call sites in core/ or tools/. It remains in
// the interface for future admin tool or dashboard wiring.
func (es *ExperienceStore) GetEnvironmentAlerts(ctx context.Context) map[string]any {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	alerts := make(map[string]any)
	for cmd, issue := range es.EnvironmentIssues {
		if !issue.IsResolved && time.Since(issue.LastSeen) < 24*time.Hour {
			alerts[cmd] = map[string]any{
				"message":   issue.Message,
				"count":     issue.Count,
				"last_seen": issue.LastSeen,
			}
		}
	}
	return alerts
}

func (s *ExperienceStore) UpsertDecisionPrecedent(ctx context.Context, node *schemas.DecisionNode) error {
	if node == nil || node.ID == "" {
		return nil
	}

	s.Mu.Lock()
	if s.DecisionPrecedents == nil {
		s.DecisionPrecedents = make(map[string]*schemas.DecisionNode)
	}
	s.DecisionPrecedents[node.ID] = node
	cli := s.embeddingClient
	s.Mu.Unlock()

	// Async embedding if client available
	if cli != nil && len(node.Embedding) == 0 && node.Reasoning != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			emb, model, err := cli.EmbedWithModel(ctx, node.Reasoning)
			if err == nil {
				s.Mu.Lock()
				if n, ok := s.DecisionPrecedents[node.ID]; ok {
					n.Embedding = emb
					n.EmbeddingModel = model
				}
				s.Mu.Unlock()
			}
		}()
	}

	return nil
}

func (s *ExperienceStore) QueryDecisionPrecedents(ctx context.Context, intent string, limit int) []*schemas.DecisionNode {
	s.Mu.RLock()
	cli := s.embeddingClient
	precedents := make([]*schemas.DecisionNode, 0, len(s.DecisionPrecedents))
	for _, p := range s.DecisionPrecedents {
		precedents = append(precedents, p)
	}
	s.Mu.RUnlock()

	if cli == nil || intent == "" || len(precedents) == 0 {
		return nil
	}

	// Embed the intent
	emb, _, err := cli.EmbedWithModel(ctx, intent)
	if err != nil {
		return nil
	}

	type ScoredNode struct {
		Node  *schemas.DecisionNode
		Score float64
	}
	var scored []ScoredNode

	for _, p := range precedents {
		if len(p.Embedding) > 0 {
			score := cosineSimilarity(emb, p.Embedding)
			if score > 0.7 { // High threshold for precedents
				scored = append(scored, ScoredNode{Node: p, Score: score})
			}
		}
	}

	// Sort by score descending
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}

	result := make([]*schemas.DecisionNode, len(scored))
	for i, s := range scored {
		result[i] = s.Node
	}
	return result
}

// QueryRelevantAntiPatterns returns anti-pattern precedents whose AntiPattern,
// Symptom, or CorrectPattern text contains any of the keywords extracted from
// the task intent. Ordered by confidence, capped by limit. Used by
// buildSystemPrompt to surface historical pitfalls to subagents in the
// ProtectedZone (zero-compression).
func (es *ExperienceStore) QueryRelevantAntiPatterns(intent string, limit int) []AntiPatternPrecedent {
	if es.AntiPatternStore == nil || intent == "" {
		return nil
	}

	words := strings.Fields(strings.ToLower(intent))
	for _, keyword := range words {
		if len(keyword) < 4 {
			continue
		}
		results := es.AntiPatternStore.QueryByKeyword(keyword, limit)
		if len(results) > 0 {
			return results
		}
	}

	// FIX (2026-09-15): removed TopConfidence fallback. Previously, when no
	// keywords from the task intent matched any stored anti-pattern, this
	// returned the globally highest-confidence anti-patterns regardless of
	// relevance — causing cross-task/cross-domain pollution where an
	// anti-pattern from one project (e.g. "don't use raw SQL") would be
	// injected into an unrelated task that shares no keywords with it. Now
	// returns nil: if no keywords match, the anti-pattern is not relevant.
	return nil
}
