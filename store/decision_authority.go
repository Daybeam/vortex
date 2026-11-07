package store

import "time"

// DecisionAuthorityStats summarises the proposer/decider distribution and
// judgment density over a time window. Mirrors Atria Dawn's authority
// separation metrics (arXiv:2609.15818).
type DecisionAuthorityStats struct {
	ProposerDistribution map[string]int `json:"proposer_distribution"`
	DeciderDistribution  map[string]int `json:"decider_distribution"`
	JudgmentDensity      float64        `json:"judgment_density"`
}

// JudgmentDensity returns agent actions per human intervention over the
// given time window. Mirrors Atria Dawn's 28.5 ratio as a surface
// indicator, NOT autonomy — a rising ratio means each human judgment
// propagates through more agent actions, not that agents decide more.
// Returns 0 when there are no interventions in the window.
func (es *ExperienceStore) JudgmentDensity(since time.Time) float64 {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	agentActions := 0
	interventions := 0
	for _, d := range es.DecisionOutcomes {
		if d.Timestamp.Before(since) {
			continue
		}
		if d.Decider == "human" || d.Intervention != "" {
			interventions++
		} else {
			agentActions++
		}
	}
	if interventions == 0 {
		return 0
	}
	return float64(agentActions) / float64(interventions)
}

// GetDecisionAuthorityStats returns proposer/decider distributions and
// judgment density over the given time window. The proposer distribution
// should show wider variance than the decider distribution — if not, the
// authority fields are being populated wrong (per Atria Dawn §validation).
func (es *ExperienceStore) GetDecisionAuthorityStats(since time.Time) *DecisionAuthorityStats {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	stats := &DecisionAuthorityStats{
		ProposerDistribution: make(map[string]int),
		DeciderDistribution:  make(map[string]int),
	}

	agentActions := 0
	interventions := 0

	for _, d := range es.DecisionOutcomes {
		if d.Timestamp.Before(since) {
			continue
		}
		if d.Proposer != "" {
			stats.ProposerDistribution[d.Proposer]++
		}
		if d.Decider != "" {
			stats.DeciderDistribution[d.Decider]++
		}
		if d.Decider == "human" || d.Intervention != "" {
			interventions++
		} else {
			agentActions++
		}
	}

	if interventions > 0 {
		stats.JudgmentDensity = float64(agentActions) / float64(interventions)
	}

	return stats
}
