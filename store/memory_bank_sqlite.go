package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sort"
	"time"
)

type SQLiteMemoryBankBackend struct {
	db *sql.DB
}

func NewSQLiteMemoryBankBackend(db *sql.DB) *SQLiteMemoryBankBackend {
	return &SQLiteMemoryBankBackend{db: db}
}

func (b *SQLiteMemoryBankBackend) SaveItem(ctx context.Context, category, key, content string, metadata map[string]any) error {
	metaJSON, _ := json.Marshal(metadata)
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO memory_bank (category, key, content, metadata_json, last_updated)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(category, key) DO UPDATE SET
			content=excluded.content,
			metadata_json=excluded.metadata_json,
			last_updated=excluded.last_updated
	`, category, key, content, string(metaJSON), time.Now())
	return err
}

func (b *SQLiteMemoryBankBackend) LoadCategory(ctx context.Context, category string) (map[string]string, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT key, content FROM memory_bank WHERE category = ?`, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make(map[string]string)
	for rows.Next() {
		var key, content string
		if err := rows.Scan(&key, &content); err != nil {
			return nil, err
		}
		results[key] = content
	}
	return results, nil
}

// SearchItems returns memory bank items similar to the given query vector.
func (b *SQLiteMemoryBankBackend) SearchItems(ctx context.Context, category string, queryVec []float32, limit int) (map[string]string, error) {
	// Pure SQL version (brute-force cosine similarity in Go layer for now)
	rows, err := b.db.QueryContext(ctx, `SELECT key, content, embedding FROM memory_bank WHERE (category = ? OR ? = '') AND embedding IS NOT NULL`, category, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		key     string
		content string
		score   float64
	}
	var candidates []scored

	for rows.Next() {
		var key, content string
		var emb []byte
		if err := rows.Scan(&key, &content, &emb); err != nil {
			log.Printf("WARN: memory_bank: skipping row with scan error: %v", err) // audit M3: was silent skip
			continue
		}
		vec := bytesToFloat32(emb)
		score := cosineSimilarity(queryVec, vec)
		if score > 0.1 {
			candidates = append(candidates, scored{key: key, content: content, score: score})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	results := make(map[string]string)
	for _, c := range candidates {
		results[c.key] = c.content
	}
	return results, nil
}
