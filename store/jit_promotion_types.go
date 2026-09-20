package store

import "time"

// JITCandidate represents a composition pattern that has passed the
// sample-count threshold but has not yet been audited for permanent
// promotion. ADDED (2026-09-06) as part of the JIT Audit lifecycle.
type JITCandidate struct {
	ID             string    `json:"id"`
	SequenceKey    string    `json:"sequence_key"`
	SampleCount    int       `json:"sample_count"`
	AvgConfidence  float64   `json:"avg_confidence"`
	Status         string    `json:"status"` // "pending" | "audited" | "rejected"
	PromotionNotes string    `json:"promotion_notes,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	LastSeen       time.Time `json:"last_seen"`
}

// PromotionAuditLog captures the history of JIT tool promotions.
type PromotionAuditLog struct {
	CandidateID string    `json:"candidate_id"`
	Action      string    `json:"action"`  // "promoted" | "rejected"
	Auditor     string    `json:"auditor"` // "system" | "admin"
	Reason      string    `json:"reason"`
	Timestamp   time.Time `json:"timestamp"`
}
