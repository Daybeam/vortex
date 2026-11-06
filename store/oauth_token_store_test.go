package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newOAuthTokenTestDB(t *testing.T) *OAuthTokenStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_oauth.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewOAuthTokenStore(db)
}

func TestOAuthTokenStore_SaveAndLoad(t *testing.T) {
	s := newOAuthTokenTestDB(t)
	ctx := context.Background()
	expiresAt := time.Now().Add(1 * time.Hour)

	if err := s.SaveToken(ctx, "user-1", "client-a", "https://token.example", "access-123", "refresh-456", expiresAt); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	access, refresh, gotExpires, found, err := s.LoadToken(ctx, "user-1", "client-a", "https://token.example")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if !found {
		t.Fatal("expected token to be found")
	}
	if access != "access-123" {
		t.Errorf("expected access-123, got %s", access)
	}
	if refresh != "refresh-456" {
		t.Errorf("expected refresh-456, got %s", refresh)
	}
	if gotExpires.Before(time.Now()) {
		t.Errorf("expected future expiry, got %v", gotExpires)
	}
}

func TestOAuthTokenStore_NotFound(t *testing.T) {
	s := newOAuthTokenTestDB(t)
	ctx := context.Background()

	_, _, _, found, err := s.LoadToken(ctx, "nonexistent", "client", "endpoint")
	if err != nil {
		t.Fatalf("LoadToken error: %v", err)
	}
	if found {
		t.Error("expected found=false for nonexistent token")
	}
}

func TestOAuthTokenStore_PerUserIsolation(t *testing.T) {
	s := newOAuthTokenTestDB(t)
	ctx := context.Background()
	expiresAt := time.Now().Add(1 * time.Hour)

	// Save tokens for two different users with the same client.
	if err := s.SaveToken(ctx, "user-A", "client-x", "ep", "token-A", "refresh-A", expiresAt); err != nil {
		t.Fatalf("SaveToken A: %v", err)
	}
	if err := s.SaveToken(ctx, "user-B", "client-x", "ep", "token-B", "refresh-B", expiresAt); err != nil {
		t.Fatalf("SaveToken B: %v", err)
	}

	accessA, _, _, foundA, _ := s.LoadToken(ctx, "user-A", "client-x", "ep")
	if !foundA || accessA != "token-A" {
		t.Errorf("expected token-A for user-A, got %s (found=%v)", accessA, foundA)
	}

	accessB, _, _, foundB, _ := s.LoadToken(ctx, "user-B", "client-x", "ep")
	if !foundB || accessB != "token-B" {
		t.Errorf("expected token-B for user-B, got %s (found=%v)", accessB, foundB)
	}
}

func TestOAuthTokenStore_Upsert(t *testing.T) {
	s := newOAuthTokenTestDB(t)
	ctx := context.Background()
	expiresAt := time.Now().Add(1 * time.Hour)

	// Save initial token.
	if err := s.SaveToken(ctx, "user-1", "client", "ep", "old-token", "old-refresh", expiresAt); err != nil {
		t.Fatalf("SaveToken initial: %v", err)
	}
	// Upsert with new token.
	if err := s.SaveToken(ctx, "user-1", "client", "ep", "new-token", "new-refresh", expiresAt); err != nil {
		t.Fatalf("SaveToken upsert: %v", err)
	}

	access, refresh, _, found, _ := s.LoadToken(ctx, "user-1", "client", "ep")
	if !found || access != "new-token" {
		t.Errorf("expected new-token after upsert, got %s", access)
	}
	if refresh != "new-refresh" {
		t.Errorf("expected new-refresh after upsert, got %s", refresh)
	}
}

func TestOAuthTokenStore_DeleteAllForOwner(t *testing.T) {
	s := newOAuthTokenTestDB(t)
	ctx := context.Background()
	expiresAt := time.Now().Add(1 * time.Hour)

	// Save tokens for two users.
	_ = s.SaveToken(ctx, "user-A", "client-1", "ep1", "t1", "r1", expiresAt)
	_ = s.SaveToken(ctx, "user-A", "client-2", "ep2", "t2", "r2", expiresAt)
	_ = s.SaveToken(ctx, "user-B", "client-1", "ep1", "t3", "r3", expiresAt)

	// Delete all for user-A.
	if err := s.DeleteAllForOwner(ctx, "user-A"); err != nil {
		t.Fatalf("DeleteAllForOwner: %v", err)
	}

	// user-A tokens should be gone.
	_, _, _, foundA, _ := s.LoadToken(ctx, "user-A", "client-1", "ep1")
	if foundA {
		t.Error("expected user-A token to be deleted")
	}

	// user-B token should still exist.
	_, _, _, foundB, _ := s.LoadToken(ctx, "user-B", "client-1", "ep1")
	if !foundB {
		t.Error("expected user-B token to still exist")
	}
}

func TestOAuthTokenStore_OperatorLevel(t *testing.T) {
	s := newOAuthTokenTestDB(t)
	ctx := context.Background()
	expiresAt := time.Now().Add(1 * time.Hour)

	// ownerID="" means operator-level (shared).
	if err := s.SaveToken(ctx, "", "client", "ep", "op-token", "op-refresh", expiresAt); err != nil {
		t.Fatalf("SaveToken operator: %v", err)
	}

	access, _, _, found, _ := s.LoadToken(ctx, "", "client", "ep")
	if !found || access != "op-token" {
		t.Errorf("expected op-token for operator-level, got %s (found=%v)", access, found)
	}
}
