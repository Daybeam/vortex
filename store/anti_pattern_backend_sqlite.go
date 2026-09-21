package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

type SQLiteAntiPatternBackend struct {
	db *sql.DB
}

func NewSQLiteAntiPatternBackend(db *sql.DB) *SQLiteAntiPatternBackend {
	return &SQLiteAntiPatternBackend{db: db}
}

func (b *SQLiteAntiPatternBackend) Upsert(ctx context.Context, p AntiPatternPrecedent) error {
	tags, _ := json.Marshal(p.Tags)
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO anti_patterns (
			id, anti_pattern, correct_pattern, trigger_condition, symptom, category,
			confidence, tags_json, source_text, embedding, embedding_model, last_seen, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			anti_pattern=excluded.anti_pattern,
			correct_pattern=excluded.correct_pattern,
			trigger_condition=excluded.trigger_condition,
			symptom=excluded.symptom,
			category=excluded.category,
			confidence=excluded.confidence,
			tags_json=excluded.tags_json,
			source_text=excluded.source_text,
			embedding=excluded.embedding,
			embedding_model=excluded.embedding_model,
			last_seen=excluded.last_seen
	`, p.ID, p.AntiPattern, p.CorrectPattern, p.TriggerCondition, p.Symptom, p.Category,
		p.Confidence, string(tags), p.SourceText, float32ToBytes(p.Embedding), p.EmbeddingModel, p.LastSeen, p.CreatedAt)
	return err
}

func (b *SQLiteAntiPatternBackend) LoadAll(ctx context.Context) ([]AntiPatternPrecedent, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, anti_pattern, correct_pattern, trigger_condition, symptom, category,
		       confidence, tags_json, source_text, embedding, embedding_model, last_seen, created_at
		FROM anti_patterns
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []AntiPatternPrecedent
	for rows.Next() {
		var p AntiPatternPrecedent
		var tags string
		var emb []byte
		err := rows.Scan(
			&p.ID, &p.AntiPattern, &p.CorrectPattern, &p.TriggerCondition, &p.Symptom, &p.Category,
			&p.Confidence, &tags, &p.SourceText, &emb, &p.EmbeddingModel, &p.LastSeen, &p.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(tags), &p.Tags)
		p.Embedding = bytesToFloat32(emb)
		results = append(results, p)
	}
	return results, nil
}
