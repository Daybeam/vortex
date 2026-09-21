package store

import (
	"time"
)

const confidenceThresholdFallback = 0.75

// maxDecisionOutcomes bounds the DecisionOutcomes history so it doesn't grow
// unbounded across the lifetime of a long-running orchestrator process.
const maxDecisionOutcomesFallback = 2000

type TaskPattern struct {
	ID           string           `json:"pattern_id"`
	TaskID       string           `json:"task_id"`
	TaskType     string           `json:"task_type"`
	StepSequence []map[string]any `json:"step_sequence"`
	// SequenceKey is a canonical signature of StepSequence (role_id +
	// capability + sorted skills, per step, joined in order), used by
	// upsertTaskPattern to detect "is this the same fixed composition as an
	// existing pattern" so SampleCount can actually accumulate across
	// repeated occurrences instead of every call creating a fresh pattern
	// with SampleCount permanently pinned at 1. ADDED (2026-07-28).
	SequenceKey   string    `json:"sequence_key,omitempty"`
	SampleCount   int       `json:"sample_count"`
	AvgConfidence float64   `json:"avg_confidence"`
	AvgTurnsUsed  float64   `json:"avg_turns_used,omitempty"`
	LastSeen      time.Time `json:"last_seen"`
	IsSeed        bool      `json:"is_seed"`

	// Perception 2.0 (Vector Resilience)
	SourceText     string    `json:"source_text,omitempty"`
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`

	// Ephemeral ranking
	MatchScore float64 `json:"match_score,omitempty"`
}

func (p TaskPattern) PatternID() string {
	return p.ID
}

func (p TaskPattern) GetTaskType() string {
	return p.TaskType
}

type RoleProfile struct {
	RoleID                    string     `json:"role_id"`
	TotalRuns                 int        `json:"total_runs"`
	SuccessCount              int        `json:"success_count"`
	PartialCount              int        `json:"partial_count"`
	FailureCount              int        `json:"failure_count"`
	AvgConfidence             float64    `json:"avg_confidence"`
	CommonMissingContext      []string   `json:"common_missing_context"`
	HighConfidenceSkillCombos [][]string `json:"high_confidence_skill_combos"`
	FailureSkillCombos        [][]string `json:"failure_skill_combos"`
	LastUpdated               time.Time  `json:"last_updated"`
}

type DecisionOutcome struct {
	DecisionType    string    `json:"decision_type"`
	RoleID          string    `json:"role_id"`
	Capability      string    `json:"capability"`
	Choice          string    `json:"choice"`
	Resolved        bool      `json:"resolved"`
	FinalConfidence *float64  `json:"final_confidence,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
}

type SkillAffinity struct {
	BaseCapability  string    `json:"base_capability"`
	AddedSkill      string    `json:"added_skill"`
	ConfidenceDelta float64   `json:"confidence_delta"`
	SampleCount     int       `json:"sample_count"`
	LastUpdated     time.Time `json:"last_updated"`
	IsSeed          bool      `json:"is_seed"`
}

type GeneratedSkill struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Capability  string    `json:"capability"`
	UsageCount  int       `json:"usage_count"`
	SuccessRate float64   `json:"success_rate"`
	LastUsed    time.Time `json:"last_used"`
	IsVerified  bool      `json:"is_verified"`
	Criticality float64   `json:"criticality"`
	// ParentID (ADDED 2026-08-01): empty = root skill for its Capability;
	// non-empty = variant sibling of that skill ID. Roots are never pruned
	// by PruneSkills, guaranteeing a permanent SelectSkill candidate.
	ParentID string `json:"parent_id,omitempty"`

	// Evolution 2.0 (ADDED 2026-08-16)
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Shell         string `json:"shell,omitempty"`          // Shell version (ADDED 2026-08-20)
	FailureSignal string `json:"failure_signal,omitempty"` // Triggering error signal

	// Perception 2.0
	SourceText     string    `json:"source_text,omitempty"`
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`

	// Metabolic Pruning (ADDED 2026-09-13) — see docs/METABOLIC_PRUNING_DESIGN.md
	TokenCostTotal int     `json:"token_cost_total,omitempty"`
	AvgLatencyMs   float64 `json:"avg_latency_ms,omitempty"`
	MetabolicROI   float64 `json:"metabolic_roi,omitempty"`
	SkillStatus    string  `json:"skill_status,omitempty"` // active, dormant, pruned
}

type RouteWeight struct {
	RoleID       string    `json:"role_id"`
	ModelID      string    `json:"model_id"`
	Capability   string    `json:"capability"`
	SkillID      string    `json:"skill_id"`
	TotalRuns    int       `json:"total_runs"`
	SuccessCount int       `json:"success_count"`
	AvgScore     float64   `json:"avg_score"`
	Weight       float64   `json:"weight"`
	LastUpdated  time.Time `json:"last_updated"`
}

// StatePotential tracks the empirical success rate of trajectories passing
// through a discrete environment state (Tool + Observation Hash). Inspired
// by PGPO (Potential-Guided Policy Optimization). ADDED (2026-09-08) for
// State-Potential Dynamic Routing.
type StatePotential struct {
	StateHash    string    `json:"state_hash"`
	ToolID       string    `json:"tool_id"`
	TotalRuns    int       `json:"total_runs"`
	SuccessCount int       `json:"success_count"`
	Potential    float64   `json:"potential"` // SuccessCount / TotalRuns
	LastUpdated  time.Time `json:"last_updated"`
}

// CooccurrenceEntry tracks how often two tools/capabilities are used
// consecutively within the same task. Used for Compound Skill auto-crystallization.
// ADDED (2026-09-13) — see docs/COMPOUND_SKILLS_DESIGN.md
type CooccurrenceEntry struct {
	ToolA        string    `json:"tool_a"`
	ToolB        string    `json:"tool_b"`
	CoCount      int       `json:"co_count"`
	SuccessCount int       `json:"success_count"`
	LastUpdated  time.Time `json:"last_updated"`
}

// SuccessRate returns success_count / co_count, or 0 if no data.
func (c *CooccurrenceEntry) SuccessRate() float64 {
	if c.CoCount == 0 {
		return 0
	}
	return float64(c.SuccessCount) / float64(c.CoCount)
}

// FileExperienceBackend implements IExperienceBackend using local files.
