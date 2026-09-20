package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newCapProfileTestDB(t *testing.T) (*CapabilityProfileStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "capprofile_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("InitDB: %v", err)
	}
	store := NewCapabilityProfileStore(db)
	return store, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func TestRecordOutcome_FirstRecord(t *testing.T) {
	store, cleanup := newCapProfileTestDB(t)
	defer cleanup()
	ctx := context.Background()

	err := store.RecordOutcome(ctx, "gpt-4o", "refactor", true, 5.0, 1200.0, 0.01)
	if err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	p, err := store.GetProfile(ctx, "gpt-4o", "refactor")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p == nil {
		t.Fatal("expected profile, got nil")
	}
	if p.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1", p.TotalRuns)
	}
	if p.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", p.SuccessCount)
	}
	if p.SuccessRate != 1.0 {
		t.Errorf("SuccessRate = %f, want 1.0", p.SuccessRate)
	}
	if p.AvgTurnsUsed != 5.0 {
		t.Errorf("AvgTurnsUsed = %f, want 5.0", p.AvgTurnsUsed)
	}
}

func TestRecordOutcome_IncrementalUpdate(t *testing.T) {
	store, cleanup := newCapProfileTestDB(t)
	defer cleanup()
	ctx := context.Background()

	_ = store.RecordOutcome(ctx, "gpt-4o", "refactor", true, 4.0, 1000.0, 0.01)
	_ = store.RecordOutcome(ctx, "gpt-4o", "refactor", false, 8.0, 2000.0, 0.02)
	_ = store.RecordOutcome(ctx, "gpt-4o", "refactor", true, 6.0, 1500.0, 0.015)

	p, _ := store.GetProfile(ctx, "gpt-4o", "refactor")
	if p.TotalRuns != 3 {
		t.Errorf("TotalRuns = %d, want 3", p.TotalRuns)
	}
	if p.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2", p.SuccessCount)
	}
	if p.SuccessRate < 0.66 || p.SuccessRate > 0.67 {
		t.Errorf("SuccessRate = %f, want ~0.667", p.SuccessRate)
	}
	expectedAvgTurns := (4.0 + 8.0 + 6.0) / 3.0
	if p.AvgTurnsUsed < expectedAvgTurns-0.01 || p.AvgTurnsUsed > expectedAvgTurns+0.01 {
		t.Errorf("AvgTurnsUsed = %f, want ~%f", p.AvgTurnsUsed, expectedAvgTurns)
	}
}

func TestGetProfile_NotFound(t *testing.T) {
	store, cleanup := newCapProfileTestDB(t)
	defer cleanup()
	ctx := context.Background()

	p, err := store.GetProfile(ctx, "unknown", "unknown")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p != nil {
		t.Fatal("expected nil for non-existent profile")
	}
}

func TestGetProfilesForModel(t *testing.T) {
	store, cleanup := newCapProfileTestDB(t)
	defer cleanup()
	ctx := context.Background()

	_ = store.RecordOutcome(ctx, "gpt-4o", "refactor", true, 5.0, 1000.0, 0.01)
	_ = store.RecordOutcome(ctx, "gpt-4o", "analysis", true, 3.0, 800.0, 0.005)
	_ = store.RecordOutcome(ctx, "claude-3-5-sonnet", "refactor", true, 4.0, 1200.0, 0.02)

	profiles, err := store.GetProfilesForModel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("GetProfilesForModel: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}
}

func TestGetAllProfiles(t *testing.T) {
	store, cleanup := newCapProfileTestDB(t)
	defer cleanup()
	ctx := context.Background()

	_ = store.RecordOutcome(ctx, "gpt-4o", "refactor", true, 5.0, 1000.0, 0.01)
	_ = store.RecordOutcome(ctx, "claude-3-5-sonnet", "analysis", false, 10.0, 2000.0, 0.03)

	all, err := store.GetAllProfiles(ctx)
	if err != nil {
		t.Fatalf("GetAllProfiles: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(all))
	}
}

func TestUpdateTheta(t *testing.T) {
	store, cleanup := newCapProfileTestDB(t)
	defer cleanup()
	ctx := context.Background()

	_ = store.RecordOutcome(ctx, "gpt-4o", "refactor", true, 5.0, 1000.0, 0.01)

	err := store.UpdateTheta(ctx, "gpt-4o", "refactor", 1.75)
	if err != nil {
		t.Fatalf("UpdateTheta: %v", err)
	}

	p, _ := store.GetProfile(ctx, "gpt-4o", "refactor")
	if p.Theta != 1.75 {
		t.Errorf("Theta = %f, want 1.75", p.Theta)
	}
}

func TestNewCapabilityProfileStore_NilDB(t *testing.T) {
	s := NewCapabilityProfileStore(nil)
	if s != nil {
		t.Fatal("expected nil store for nil db")
	}
}
