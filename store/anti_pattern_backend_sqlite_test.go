package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteAntiPatternBackend(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "ap_test")
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteAntiPatternBackend(db)
	ctx := context.Background()

	p := AntiPatternPrecedent{
		ID:               "prec_1",
		AntiPattern:      "Bad Way",
		CorrectPattern:   "Good Way",
		TriggerCondition: "Condition X",
		Symptom:          "Error Y",
		Category:         "test",
		Confidence:       0.95,
		Tags:             []string{"tag1", "tag2"},
		SourceText:       "Original text",
		Embedding:        []float32{0.5, 0.6},
		CreatedAt:        time.Now().Round(time.Second),
		LastSeen:         time.Now().Round(time.Second),
	}

	// 1. Upsert
	if err := backend.Upsert(ctx, p); err != nil {
		t.Errorf("Upsert failed: %v", err)
	}

	// 2. LoadAll
	list, err := backend.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != p.ID {
		t.Errorf("LoadAll mismatch: got %v", list)
	}
	if len(list[0].Tags) != 2 || list[0].Tags[0] != "tag1" {
		t.Errorf("Tags mismatch: got %v", list[0].Tags)
	}
	if len(list[0].Embedding) != 2 || list[0].Embedding[0] != 0.5 {
		t.Errorf("Embedding mismatch: got %v", list[0].Embedding)
	}
}

func TestMemoryBankVectorSearch(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "mb_search_test")
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteMemoryBankBackend(db)
	ctx := context.Background()

	// Insert some items with embeddings
	backend.SaveItem(ctx, "goals", "g1", "Goal 1", nil)
	// Manually update embedding since SaveItem doesn't take it yet
	db.Exec(`UPDATE memory_bank SET embedding = ? WHERE key = 'g1'`, float32ToBytes([]float32{1.0, 0.0}))

	backend.SaveItem(ctx, "goals", "g2", "Goal 2", nil)
	db.Exec(`UPDATE memory_bank SET embedding = ? WHERE key = 'g2'`, float32ToBytes([]float32{0.0, 1.0}))

	// Search
	results, err := backend.SearchItems(ctx, "goals", []float32{0.9, 0.1}, 1)
	if err != nil {
		t.Fatalf("SearchItems failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
	if _, ok := results["g1"]; !ok {
		t.Errorf("Expected g1 to be found (most similar to [1,0])")
	}
}
