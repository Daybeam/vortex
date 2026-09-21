package store

import (
	"context"
	"database/sql"
	"time"
)

// ModelCapabilityKey uniquely identifies a per-model per-capability profile.
type ModelCapabilityKey struct {
	ModelID    string
	Capability string
}

// ModelCapabilityProfile tracks per-model per-capability execution telemetry
// for intelligent routing, Pareto frontier selection, and IRT turn budgeting.
//
// See docs/architecture/MODEL_CAPABILITY_AND_INTELLIGENT_ROUTING_ROADMAP.md §2 Module A.
type ModelCapabilityProfile struct {
	ModelID      string    `json:"model_id"`
	Capability   string    `json:"capability"`
	TotalRuns    int       `json:"total_runs"`
	SuccessCount int       `json:"success_count"`
	SuccessRate  float64   `json:"success_rate"`
	AvgTurnsUsed float64   `json:"avg_turns_used"`
	AvgLatencyMs float64   `json:"avg_latency_ms"`
	AvgTokenCost float64   `json:"avg_token_cost"`
	Theta        float64   `json:"theta"`
	LastUpdated  time.Time `json:"last_updated"`
}

// CapabilityProfileStore manages per-model per-capability telemetry persistence.
type CapabilityProfileStore struct {
	db *sql.DB
}

// NewCapabilityProfileStore creates a store backed by the given SQLite connection.
// Returns nil if db is nil (file mode — profiles stay in-memory only).
func NewCapabilityProfileStore(db *sql.DB) *CapabilityProfileStore {
	if db == nil {
		return nil
	}
	return &CapabilityProfileStore{db: db}
}

// RecordOutcome incrementally updates a capability profile with a new execution result.
// SuccessRate is derived from TotalRuns and SuccessCount. Theta is not modified here
// — it is estimated separately via batch recalculation (Elo/Rasch).
func (s *CapabilityProfileStore) RecordOutcome(ctx context.Context, modelID, capability string, success bool, turnsUsed float64, latencyMs float64, tokenCost float64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_capability_profiles (model_id, capability, total_runs, success_count, avg_turns_used, avg_latency_ms, avg_token_cost, theta, last_updated)
		VALUES (?, ?, 1, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(model_id, capability) DO UPDATE SET
			total_runs     = total_runs + 1,
			success_count  = success_count + ?,
			avg_turns_used = (avg_turns_used * total_runs + ?) / (total_runs + 1),
			avg_latency_ms = (avg_latency_ms * total_runs + ?) / (total_runs + 1),
			avg_token_cost = (avg_token_cost * total_runs + ?) / (total_runs + 1),
			last_updated   = CURRENT_TIMESTAMP
	`, modelID, capability, boolToInt(success), turnsUsed, latencyMs, tokenCost,
		boolToInt(success), turnsUsed, latencyMs, tokenCost)
	return err
}

// GetProfile retrieves a single capability profile. Returns nil if not found.
func (s *CapabilityProfileStore) GetProfile(ctx context.Context, modelID, capability string) (*ModelCapabilityProfile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT model_id, capability, total_runs, success_count, avg_turns_used, avg_latency_ms, avg_token_cost, theta, last_updated
		FROM model_capability_profiles WHERE model_id = ? AND capability = ?
	`, modelID, capability)

	var p ModelCapabilityProfile
	err := row.Scan(&p.ModelID, &p.Capability, &p.TotalRuns, &p.SuccessCount, &p.AvgTurnsUsed, &p.AvgLatencyMs, &p.AvgTokenCost, &p.Theta, &p.LastUpdated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.SuccessRate = safeDiv(p.SuccessCount, p.TotalRuns)
	return &p, nil
}

// GetProfilesForModel returns all capability profiles for a given model.
func (s *CapabilityProfileStore) GetProfilesForModel(ctx context.Context, modelID string) ([]ModelCapabilityProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model_id, capability, total_runs, success_count, avg_turns_used, avg_latency_ms, avg_token_cost, theta, last_updated
		FROM model_capability_profiles WHERE model_id = ?
	`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelCapabilityProfile
	for rows.Next() {
		var p ModelCapabilityProfile
		if err := rows.Scan(&p.ModelID, &p.Capability, &p.TotalRuns, &p.SuccessCount, &p.AvgTurnsUsed, &p.AvgLatencyMs, &p.AvgTokenCost, &p.Theta, &p.LastUpdated); err != nil {
			return nil, err
		}
		p.SuccessRate = safeDiv(p.SuccessCount, p.TotalRuns)
		result = append(result, p)
	}
	return result, rows.Err()
}

// GetAllProfiles returns all capability profiles across all models.
func (s *CapabilityProfileStore) GetAllProfiles(ctx context.Context) ([]ModelCapabilityProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model_id, capability, total_runs, success_count, avg_turns_used, avg_latency_ms, avg_token_cost, theta, last_updated
		FROM model_capability_profiles
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelCapabilityProfile
	for rows.Next() {
		var p ModelCapabilityProfile
		if err := rows.Scan(&p.ModelID, &p.Capability, &p.TotalRuns, &p.SuccessCount, &p.AvgTurnsUsed, &p.AvgLatencyMs, &p.AvgTokenCost, &p.Theta, &p.LastUpdated); err != nil {
			return nil, err
		}
		p.SuccessRate = safeDiv(p.SuccessCount, p.TotalRuns)
		result = append(result, p)
	}
	return result, rows.Err()
}

// UpdateTheta sets the IRT ability parameter for a specific (model, capability) pair.
// This is called by the batch theta estimator (Elo/Rasch), not by per-task recording.
func (s *CapabilityProfileStore) UpdateTheta(ctx context.Context, modelID, capability string, theta float64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE model_capability_profiles SET theta = ?, last_updated = CURRENT_TIMESTAMP
		WHERE model_id = ? AND capability = ?
	`, theta, modelID, capability)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func safeDiv(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
