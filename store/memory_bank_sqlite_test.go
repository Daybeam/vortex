package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteMemoryBankBackend(t *testing.T) {
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteMemoryBankBackend(db)
	ctx := context.Background()

	// 1. SaveItem
	if err := backend.SaveItem(ctx, "active_context", "raw", "# Goals\n- Test", nil); err != nil {
		t.Errorf("SaveItem failed: %v", err)
	}

	// 2. LoadCategory
	items, err := backend.LoadCategory(ctx, "active_context")
	if err != nil {
		t.Fatalf("LoadCategory failed: %v", err)
	}
	if items["raw"] != "# Goals\n- Test" {
		t.Errorf("Content mismatch: got %s", items["raw"])
	}
}

// TestSaveItemWithEmbedding_SearchItems is a regression test for
// DB-audit VECTOR_EMPTY: SaveItemWithEmbedding must persist the embedding
// column so SearchItems (WHERE embedding IS NOT NULL) returns results.
// Before fix, SaveItem never saved embedding.
func TestSaveItemWithEmbedding_SearchItems(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer db.Close()

	backend := NewSQLiteMemoryBankBackend(db)
	ctx := context.Background()

	// Save item with embedding
	embedding := []float32{0.1, 0.2, 0.3}
	if err := backend.SaveItemWithEmbedding(ctx, "test_cat", "key1", "content1", nil, embedding, "mock"); err != nil {
		t.Fatalf("SaveItemWithEmbedding failed: %v", err)
	}

	// SearchItems should find it via cosine similarity
	results, err := backend.SearchItems(ctx, "test_cat", embedding, 10)
	if err != nil {
		t.Fatalf("SearchItems failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchItems returned 0 results — embedding was not saved")
	}
	if _, ok := results["key1"]; !ok {
		t.Errorf("SearchItems did not return key1: got %v", results)
	}
}
