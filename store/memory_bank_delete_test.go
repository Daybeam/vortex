package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTestMemoryBankDB creates an in-memory SQLite DB with the memory_bank
// table and test data for delete method tests.
func newTestMemoryBankDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS memory_bank (
		category TEXT,
		key TEXT,
		content TEXT,
		metadata_json TEXT,
		embedding BLOB,
		embedding_model TEXT,
		last_updated DATETIME,
		PRIMARY KEY (category, key)
	)`)
	if err != nil {
		t.Fatalf("CREATE TABLE failed: %v", err)
	}
	// Insert test data.
	rows := []struct{ cat, key, content string }{
		{"user_profile", "raw", "prefers Go"},
		{"decisions", "d1", "use SQLite"},
		{"decisions", "d2", "use REST API"},
		{"progress", "p1", "done task A"},
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO memory_bank (category, key, content, last_updated) VALUES (?, ?, ?, ?)`,
			r.cat, r.key, r.content, time.Now())
		if err != nil {
			t.Fatalf("INSERT failed: %v", err)
		}
	}
	return db
}

// TestSQLiteDeleteByCategory removes all rows in the given category.
func TestSQLiteDeleteByCategory(t *testing.T) {
	db := newTestMemoryBankDB(t)
	defer db.Close()
	backend := NewSQLiteMemoryBankBackend(db)
	ctx := context.Background()

	deleted, err := backend.DeleteByCategory(ctx, "decisions")
	if err != nil {
		t.Fatalf("DeleteByCategory failed: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}

	// Verify decisions are gone.
	cat, _ := backend.LoadCategory(ctx, "decisions")
	if len(cat) != 0 {
		t.Errorf("decisions should be empty, got %d", len(cat))
	}
	// Verify other categories survive.
	up, _ := backend.LoadCategory(ctx, "user_profile")
	if len(up) != 1 {
		t.Errorf("user_profile should survive, got %d", len(up))
	}
}

// TestSQLiteDeleteByCategory_NonExistent returns 0.
func TestSQLiteDeleteByCategory_NonExistent(t *testing.T) {
	db := newTestMemoryBankDB(t)
	defer db.Close()
	backend := NewSQLiteMemoryBankBackend(db)

	deleted, err := backend.DeleteByCategory(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("DeleteByCategory failed: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 for non-existent category, got %d", deleted)
	}
}

// TestSQLiteSearchAndDelete removes rows whose content matches the query.
func TestSQLiteSearchAndDelete(t *testing.T) {
	db := newTestMemoryBankDB(t)
	defer db.Close()
	backend := NewSQLiteMemoryBankBackend(db)

	deleted, err := backend.SearchAndDelete(context.Background(), "SQLite")
	if err != nil {
		t.Fatalf("SearchAndDelete failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted (content contains 'SQLite'), got %d", deleted)
	}

	// Verify the matching row is gone.
	cat, _ := backend.LoadCategory(context.Background(), "decisions")
	if len(cat) != 1 {
		t.Errorf("expected 1 remaining decision, got %d", len(cat))
	}
}

// TestSQLiteSearchAndDelete_NoMatch returns 0.
func TestSQLiteSearchAndDelete_NoMatch(t *testing.T) {
	db := newTestMemoryBankDB(t)
	defer db.Close()
	backend := NewSQLiteMemoryBankBackend(db)

	deleted, err := backend.SearchAndDelete(context.Background(), "nonexistent_term")
	if err != nil {
		t.Fatalf("SearchAndDelete failed: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 for no match, got %d", deleted)
	}
}

// TestMemoryBankStore_DeleteUserScope deletes user_profile category + disk file.
func TestMemoryBankStore_DeleteUserScope(t *testing.T) {
	db := newTestMemoryBankDB(t)
	defer db.Close()
	backend := NewSQLiteMemoryBankBackend(db)
	dir := t.TempDir()
	mbStore := NewMemoryBankStore(dir, backend)

	deleted, err := mbStore.DeleteUserScope(context.Background())
	if err != nil {
		t.Fatalf("DeleteUserScope failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}

	// Verify user_profile is gone from DB.
	cat, _ := backend.LoadCategory(context.Background(), "user_profile")
	if len(cat) != 0 {
		t.Errorf("user_profile should be empty, got %d", len(cat))
	}
}

// TestMemoryBankStore_SearchAndDelete delegates to backend correctly.
func TestMemoryBankStore_SearchAndDelete(t *testing.T) {
	db := newTestMemoryBankDB(t)
	defer db.Close()
	backend := NewSQLiteMemoryBankBackend(db)
	dir := t.TempDir()
	mbStore := NewMemoryBankStore(dir, backend)

	deleted, err := mbStore.SearchAndDelete(context.Background(), "REST")
	if err != nil {
		t.Fatalf("SearchAndDelete failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}
}
