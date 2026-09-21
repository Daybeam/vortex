package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/daybeam/vortex/schemas"
)

type SQLiteExperienceBackend struct {
	db *sql.DB
}

func NewSQLiteExperienceBackend(db *sql.DB) *SQLiteExperienceBackend {
	return &SQLiteExperienceBackend{db: db}
}

// Save implements legacy bulk save (used for migration or fallback)
func (b *SQLiteExperienceBackend) Save(ctx context.Context, data map[string]any) error {
	// In a real incremental implementation, this might be a no-op or trigger a full sync.
	// For migration, we'll iterate and call incremental saves.
	return nil
}

func (b *SQLiteExperienceBackend) Load(ctx context.Context) (map[string]any, error) {
	// Load everything from DB and reconstruct the maps for ExperienceStore memory
	data := make(map[string]any)

	// 1. TaskPatterns
	patterns := make(map[string]TaskPattern)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT id, task_type, sequence_key, sample_count, avg_confidence, last_seen, is_seed, source_text, embedding, embedding_model, raw_json FROM task_patterns`)
		if err != nil {
			return nil, fmt.Errorf("query task_patterns: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p TaskPattern
			var lastSeen time.Time
			var emb []byte
			var rawJSON string
			if err := rows.Scan(&p.ID, &p.TaskType, &p.SequenceKey, &p.SampleCount, &p.AvgConfidence, &lastSeen, &p.IsSeed, &p.SourceText, &emb, &p.EmbeddingModel, &rawJSON); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted task_patterns row: %v", err)
				continue
			}
			p.LastSeen = lastSeen
			p.Embedding = bytesToFloat32(emb)
			if err := json.Unmarshal([]byte(rawJSON), &p); err != nil {
				log.Printf("WARN: experience_backend: corrupted raw_json for task_pattern %s: %v", p.ID, err)
			}
			patterns[p.ID] = p
		}
	}
	data["task_patterns"] = patterns

	// 2. RoleProfiles
	profiles := make(map[string]*RoleProfile)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT raw_json FROM role_profiles`)
		if err != nil {
			return nil, fmt.Errorf("query role_profiles: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted role_profiles row: %v", err)
				continue
			}
			var rp RoleProfile
			if err := json.Unmarshal([]byte(raw), &rp); err != nil {
				log.Printf("WARN: experience_backend: corrupted raw_json for role_profile: %v", err)
				continue
			}
			profiles[rp.RoleID] = &rp
		}
	}
	data["role_profiles"] = profiles

	// 3. RoutingMatrix (route_weights)
	// Matrix: [RoleID][ModelID][Capability][SkillID]
	matrix := make(map[string]map[string]map[string]map[string]*RouteWeight)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT role_id, model_id, capability, skill_id, total_runs, success_count, avg_score, weight, last_updated FROM route_weights`)
		if err != nil {
			return nil, fmt.Errorf("query route_weights: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rw RouteWeight
			var lastUpdated time.Time
			if err := rows.Scan(&rw.RoleID, &rw.ModelID, &rw.Capability, &rw.SkillID, &rw.TotalRuns, &rw.SuccessCount, &rw.AvgScore, &rw.Weight, &lastUpdated); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted route_weights row: %v", err)
				continue
			}
			rw.LastUpdated = lastUpdated

			if matrix[rw.RoleID] == nil {
				matrix[rw.RoleID] = make(map[string]map[string]map[string]*RouteWeight)
			}
			if matrix[rw.RoleID][rw.ModelID] == nil {
				matrix[rw.RoleID][rw.ModelID] = make(map[string]map[string]*RouteWeight)
			}
			if matrix[rw.RoleID][rw.ModelID][rw.Capability] == nil {
				matrix[rw.RoleID][rw.ModelID][rw.Capability] = make(map[string]*RouteWeight)
			}
			matrix[rw.RoleID][rw.ModelID][rw.Capability][rw.SkillID] = &rw
		}
	}
	data["routing_matrix"] = matrix

	// 4. GeneratedSkills
	skills := make(map[string]*GeneratedSkill)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT id, capability, description, usage_count, success_rate, is_verified, criticality, parent_id, os, arch, shell, failure_signal, source_text, embedding, embedding_model, last_used FROM generated_skills`)
		if err != nil {
			return nil, fmt.Errorf("query generated_skills: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var gs GeneratedSkill
			var lastUsed time.Time
			var emb []byte
			if err := rows.Scan(&gs.ID, &gs.Capability, &gs.Description, &gs.UsageCount, &gs.SuccessRate, &gs.IsVerified, &gs.Criticality, &gs.ParentID, &gs.OS, &gs.Arch, &gs.Shell, &gs.FailureSignal, &gs.SourceText, &emb, &gs.EmbeddingModel, &lastUsed); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted generated_skills row: %v", err)
				continue
			}
			gs.LastUsed = lastUsed
			gs.Embedding = bytesToFloat32(emb)
			skills[gs.ID] = &gs
		}
	}
	data["generated_skills"] = skills

	// 5. SkillAffinities
	affinities := make(map[string]*SkillAffinity)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT base_capability, added_skill, confidence_delta, sample_count, last_updated, is_seed FROM skill_affinities`)
		if err != nil {
			return nil, fmt.Errorf("query skill_affinities: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sa SkillAffinity
			var lastUpdated time.Time
			if err := rows.Scan(&sa.BaseCapability, &sa.AddedSkill, &sa.ConfidenceDelta, &sa.SampleCount, &lastUpdated, &sa.IsSeed); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted skill_affinities row: %v", err)
				continue
			}
			sa.LastUpdated = lastUpdated
			affinities[sa.BaseCapability+":"+sa.AddedSkill] = &sa
		}
	}
	data["skill_affinity"] = affinities

	// 6. RoleAffinities
	roleAffs := make(map[string]map[string]float64)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT role_id, task_type, score FROM role_affinities`)
		if err != nil {
			return nil, fmt.Errorf("query role_affinities: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rid, tt string
			var score float64
			if err := rows.Scan(&rid, &tt, &score); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted role_affinities row: %v", err)
				continue
			}
			if roleAffs[rid] == nil {
				roleAffs[rid] = make(map[string]float64)
			}
			roleAffs[rid][tt] = score
		}
	}
	data["role_affinities"] = roleAffs

	// 7. StatePotentials (ADDED 2026-09-08)
	statePotentials := make(map[string]*StatePotential)
	{
		rows, err := b.db.QueryContext(ctx, `SELECT state_hash, tool_id, total_runs, success_count, potential, last_updated FROM state_potentials`)
		if err != nil {
			return nil, fmt.Errorf("query state_potentials: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sp StatePotential
			var lastUpdated time.Time
			if err := rows.Scan(&sp.StateHash, &sp.ToolID, &sp.TotalRuns, &sp.SuccessCount, &sp.Potential, &lastUpdated); err != nil {
				log.Printf("WARN: experience_backend: skip corrupted state_potentials row: %v", err)
				continue
			}
			sp.LastUpdated = lastUpdated
			statePotentials[sp.StateHash] = &sp
		}
	}
	data["state_potentials"] = statePotentials

	return data, nil
}

func (b *SQLiteExperienceBackend) SaveTaskPattern(ctx context.Context, p *TaskPattern) error {
	raw, _ := json.Marshal(p)
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO task_patterns (id, task_type, sequence_key, sample_count, avg_confidence, last_seen, is_seed, source_text, embedding, embedding_model, raw_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			sample_count=excluded.sample_count,
			avg_confidence=excluded.avg_confidence,
			last_seen=excluded.last_seen,
			raw_json=excluded.raw_json
	`, p.ID, p.TaskType, p.SequenceKey, p.SampleCount, p.AvgConfidence, p.LastSeen, p.IsSeed, p.SourceText, float32ToBytes(p.Embedding), p.EmbeddingModel, raw)
	return err
}

func (b *SQLiteExperienceBackend) SaveRoleProfile(ctx context.Context, rp *RoleProfile) error {
	raw, _ := json.Marshal(rp)
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO role_profiles (role_id, total_runs, success_count, partial_count, failure_count, avg_confidence, raw_json, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(role_id) DO UPDATE SET
			total_runs=excluded.total_runs,
			success_count=excluded.success_count,
			partial_count=excluded.partial_count,
			failure_count=excluded.failure_count,
			avg_confidence=excluded.avg_confidence,
			raw_json=excluded.raw_json,
			last_updated=excluded.last_updated
	`, rp.RoleID, rp.TotalRuns, rp.SuccessCount, rp.PartialCount, rp.FailureCount, rp.AvgConfidence, raw, rp.LastUpdated)
	return err
}

func (b *SQLiteExperienceBackend) SaveRouteWeight(ctx context.Context, rw *RouteWeight) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO route_weights (role_id, model_id, capability, skill_id, total_runs, success_count, avg_score, weight, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(role_id, model_id, capability, skill_id) DO UPDATE SET
			total_runs=excluded.total_runs,
			success_count=excluded.success_count,
			avg_score=excluded.avg_score,
			weight=excluded.weight,
			last_updated=excluded.last_updated
	`, rw.RoleID, rw.ModelID, rw.Capability, rw.SkillID, rw.TotalRuns, rw.SuccessCount, rw.AvgScore, rw.Weight, rw.LastUpdated)
	return err
}

func (b *SQLiteExperienceBackend) SaveGeneratedSkill(ctx context.Context, gs *GeneratedSkill) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO generated_skills (id, capability, description, usage_count, success_rate, is_verified, criticality, parent_id, os, arch, shell, failure_signal, source_text, embedding, embedding_model, last_used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			usage_count=excluded.usage_count,
			success_rate=excluded.success_rate,
			is_verified=excluded.is_verified,
			last_used=excluded.last_used
	`, gs.ID, gs.Capability, gs.Description, gs.UsageCount, gs.SuccessRate, gs.IsVerified, gs.Criticality, gs.ParentID, gs.OS, gs.Arch, gs.Shell, gs.FailureSignal, gs.SourceText, float32ToBytes(gs.Embedding), gs.EmbeddingModel, gs.LastUsed)
	return err
}

func (b *SQLiteExperienceBackend) SaveDecisionOutcome(ctx context.Context, do *DecisionOutcome) error {
	var conf sql.NullFloat64
	if do.FinalConfidence != nil {
		conf.Float64 = *do.FinalConfidence
		conf.Valid = true
	}
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO decision_outcomes (decision_type, role_id, capability, choice, resolved, final_confidence, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, do.DecisionType, do.RoleID, do.Capability, do.Choice, do.Resolved, conf, do.Timestamp)
	return err
}

func (b *SQLiteExperienceBackend) SaveSkillAffinity(ctx context.Context, sa *SkillAffinity) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO skill_affinities (base_capability, added_skill, confidence_delta, sample_count, last_updated, is_seed)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(base_capability, added_skill) DO UPDATE SET
			confidence_delta=excluded.confidence_delta,
			sample_count=excluded.sample_count,
			last_updated=excluded.last_updated
	`, sa.BaseCapability, sa.AddedSkill, sa.ConfidenceDelta, sa.SampleCount, sa.LastUpdated, sa.IsSeed)
	return err
}

func (b *SQLiteExperienceBackend) SaveEnvironmentIssue(ctx context.Context, ei *EnvironmentIssue) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO environment_issues (command, message, count, last_seen, is_resolved)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(command) DO UPDATE SET
			count=excluded.count,
			last_seen=excluded.last_seen,
			is_resolved=excluded.is_resolved
	`, ei.Command, ei.Message, ei.Count, ei.LastSeen, ei.IsResolved)
	return err
}

func (b *SQLiteExperienceBackend) SaveRoleAffinity(ctx context.Context, roleID, taskType string, score float64) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO role_affinities (role_id, task_type, score)
		VALUES (?, ?, ?)
		ON CONFLICT(role_id, task_type) DO UPDATE SET score=excluded.score
	`, roleID, taskType, score)
	return err
}

func (b *SQLiteExperienceBackend) SaveDecisionPrecedent(ctx context.Context, node *schemas.DecisionNode) error {
	raw, _ := json.Marshal(node)
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO decision_precedents (id, raw_json, embedding, embedding_model, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET raw_json=excluded.raw_json
	`, node.ID, raw, float32ToBytes(node.Embedding), node.EmbeddingModel, time.Now())
	return err
}

func (b *SQLiteExperienceBackend) SaveStatePotential(ctx context.Context, sp *StatePotential) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO state_potentials (state_hash, tool_id, total_runs, success_count, potential, last_updated)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(state_hash) DO UPDATE SET
			total_runs=excluded.total_runs,
			success_count=excluded.success_count,
			potential=excluded.potential,
			last_updated=excluded.last_updated
	`, sp.StateHash, sp.ToolID, sp.TotalRuns, sp.SuccessCount, sp.Potential, sp.LastUpdated)
	return err
}

func (b *SQLiteExperienceBackend) SaveCooccurrence(ctx context.Context, c *CooccurrenceEntry) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO tool_cooccurrence (tool_a, tool_b, co_count, success_count, last_updated)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(tool_a, tool_b) DO UPDATE SET
			co_count=tool_cooccurrence.co_count + excluded.co_count,
			success_count=tool_cooccurrence.success_count + excluded.success_count,
			last_updated=excluded.last_updated
	`, c.ToolA, c.ToolB, c.CoCount, c.SuccessCount, c.LastUpdated)
	return err
}

func (b *SQLiteExperienceBackend) GetCompoundCandidates(ctx context.Context, minCount int, minSuccessRate float64) ([]*CooccurrenceEntry, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT tool_a, tool_b, co_count, success_count, last_updated
		FROM tool_cooccurrence
		WHERE co_count >= ? AND CAST(success_count AS REAL) / CAST(co_count AS REAL) >= ?
		ORDER BY co_count DESC
	`, minCount, minSuccessRate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*CooccurrenceEntry
	for rows.Next() {
		var c CooccurrenceEntry
		if err := rows.Scan(&c.ToolA, &c.ToolB, &c.CoCount, &c.SuccessCount, &c.LastUpdated); err != nil {
			continue
		}
		results = append(results, &c)
	}
	return results, nil
}

// GetCooccurrencePartners returns all tools that co-occur with toolA
// (i.e. all tools that follow toolA in execution traces) with count >= minCount.
// Used by isOutcomeDependent to detect divergent branching.
func (b *SQLiteExperienceBackend) GetCooccurrencePartners(ctx context.Context, toolA string, minCount int) ([]*CooccurrenceEntry, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT tool_a, tool_b, co_count, success_count, last_updated
		FROM tool_cooccurrence
		WHERE tool_a = ? AND co_count >= ?
		ORDER BY co_count DESC
	`, toolA, minCount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*CooccurrenceEntry
	for rows.Next() {
		var c CooccurrenceEntry
		if err := rows.Scan(&c.ToolA, &c.ToolB, &c.CoCount, &c.SuccessCount, &c.LastUpdated); err != nil {
			continue
		}
		results = append(results, &c)
	}
	return results, nil
}

// SaveExperienceNode upserts a single experience graph node.
func (b *SQLiteExperienceBackend) SaveExperienceNode(ctx context.Context, node *ExperienceNode) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO experience_nodes (
			node_id, task_id, step_id, role_id, capability, strategy, action,
			outcome, error_signal, model_id, failure_mode, critique, confidence,
			source_text, embedding, embedding_model, timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			task_id=excluded.task_id, step_id=excluded.step_id, role_id=excluded.role_id,
			capability=excluded.capability, strategy=excluded.strategy, action=excluded.action,
			outcome=excluded.outcome, error_signal=excluded.error_signal, model_id=excluded.model_id,
			failure_mode=excluded.failure_mode, critique=excluded.critique, confidence=excluded.confidence,
			source_text=excluded.source_text, embedding=excluded.embedding,
			embedding_model=excluded.embedding_model, timestamp=excluded.timestamp
	`, node.NodeID, node.TaskID, node.StepID, node.RoleID, node.Capability, node.Strategy, node.Action,
		node.Outcome, node.ErrorSignal, node.ModelID, node.FailureMode, node.Critique, node.Confidence,
		node.SourceText, float32ToBytes(node.Embedding), node.EmbeddingModel, node.Timestamp)
	return err
}

// SaveExperienceEdge upserts a single experience graph edge.
func (b *SQLiteExperienceBackend) SaveExperienceEdge(ctx context.Context, edge ExperienceEdge) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO experience_edges (source_id, target_id, relation)
		VALUES (?, ?, ?)
		ON CONFLICT(source_id, target_id, relation) DO NOTHING
	`, edge.SourceID, edge.TargetID, edge.Relation)
	return err
}

// LoadExperienceNodes loads all experience graph nodes from SQLite.
func (b *SQLiteExperienceBackend) LoadExperienceNodes(ctx context.Context) (map[string]*ExperienceNode, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT node_id, task_id, step_id, role_id, capability, strategy, action,
			outcome, error_signal, model_id, failure_mode, critique, confidence,
			source_text, embedding, embedding_model, timestamp
		FROM experience_nodes
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make(map[string]*ExperienceNode)
	for rows.Next() {
		var n ExperienceNode
		var embedding []byte
		if err := rows.Scan(&n.NodeID, &n.TaskID, &n.StepID, &n.RoleID, &n.Capability,
			&n.Strategy, &n.Action, &n.Outcome, &n.ErrorSignal, &n.ModelID,
			&n.FailureMode, &n.Critique, &n.Confidence, &n.SourceText,
			&embedding, &n.EmbeddingModel, &n.Timestamp); err != nil {
			continue
		}
		n.Embedding = bytesToFloat32(embedding)
		nodes[n.NodeID] = &n
	}
	return nodes, nil
}

// LoadExperienceEdges loads all experience graph edges from SQLite.
func (b *SQLiteExperienceBackend) LoadExperienceEdges(ctx context.Context) ([]ExperienceEdge, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT source_id, target_id, relation FROM experience_edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []ExperienceEdge
	for rows.Next() {
		var e ExperienceEdge
		if err := rows.Scan(&e.SourceID, &e.TargetID, &e.Relation); err != nil {
			continue
		}
		edges = append(edges, e)
	}
	return edges, nil
}

// SavePromotionAuditLog appends a JIT promotion audit record.
func (b *SQLiteExperienceBackend) SavePromotionAuditLog(ctx context.Context, log *PromotionAuditLog) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO promotion_audit (candidate_id, action, auditor, reason, timestamp)
		VALUES (?, ?, ?, ?, ?)
	`, log.CandidateID, log.Action, log.Auditor, log.Reason, log.Timestamp)
	return err
}

// LoadPromotionAuditLogs loads all promotion audit records from SQLite.
func (b *SQLiteExperienceBackend) LoadPromotionAuditLogs(ctx context.Context) ([]PromotionAuditLog, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT candidate_id, action, auditor, reason, timestamp FROM promotion_audit ORDER BY timestamp DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []PromotionAuditLog
	for rows.Next() {
		var l PromotionAuditLog
		if err := rows.Scan(&l.CandidateID, &l.Action, &l.Auditor, &l.Reason, &l.Timestamp); err != nil {
			continue
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// Helpers for embedding conversion

func float32ToBytes(slice []float32) []byte {
	if slice == nil {
		return nil
	}
	b := make([]byte, len(slice)*4)
	for i, v := range slice {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

func bytesToFloat32(b []byte) []float32 {
	if len(b) == 0 {
		return nil
	}
	slice := make([]float32, len(b)/4)
	for i := 0; i < len(slice); i++ {
		slice[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return slice
}
