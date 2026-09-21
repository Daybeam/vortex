package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteMemoryBankBackend(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "mb_test")
	defer os.RemoveAll(tmpDir)

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
