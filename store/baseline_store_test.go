package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func newBaselineTestDB(t *testing.T) (*SQLiteRepo, *SQLiteBaselineStore) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return NewSQLiteRepo(db), NewBaselineStore(db)
}

func TestBaselineStore_PinAndRetrieve(t *testing.T) {
	_, store := newBaselineTestDB(t)
	ctx := context.Background()

	if err := store.PinBaseline(ctx, "task-1", "deploy_sop", "1.0.0", "engineer", "2.1.0"); err != nil {
		t.Fatalf("PinBaseline: %v", err)
	}

	b, err := store.GetBaseline(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetBaseline: %v", err)
	}
	if b == nil {
		t.Fatal("expected baseline, got nil")
	}
	if b.SOPID != "deploy_sop" || b.SOPVersion != "1.0.0" {
		t.Errorf("sop = %s/%s, want deploy_sop/1.0.0", b.SOPID, b.SOPVersion)
	}
	if b.RoleID != "engineer" || b.RoleVersion != "2.1.0" {
		t.Errorf("role = %s/%s, want engineer/2.1.0", b.RoleID, b.RoleVersion)
	}
}

func TestBaselineStore_GetBaseline_NotFound(t *testing.T) {
	_, store := newBaselineTestDB(t)

	b, err := store.GetBaseline(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b != nil {
		t.Fatal("expected nil for nonexistent task")
	}
}

func TestBaselineStore_LoadPinnedRole(t *testing.T) {
	repo, store := newBaselineTestDB(t)

	role := &config.Role{
		ID:             "engineer",
		Version:        "2.1.0",
		Author:         "human_expert",
		MutableByAgent: false,
		Name:           "Software Engineer",
		BaseCapability: "coding",
	}
	if err := repo.SyncFileToDB(map[string]*config.Role{"engineer": role}); err != nil {
		t.Fatalf("SyncFileToDB: %v", err)
	}

	pinned, err := store.LoadPinnedRole(context.Background(), "engineer", "2.1.0")
	if err != nil {
		t.Fatalf("LoadPinnedRole: %v", err)
	}
	if pinned.ID != "engineer" || pinned.Version != "2.1.0" {
		t.Errorf("got %s/%s, want engineer/2.1.0", pinned.ID, pinned.Version)
	}
	if pinned.Author != "human_expert" {
		t.Errorf("author = %s, want human_expert", pinned.Author)
	}
}

func TestBaselineStore_LoadPinnedRole_NotFound(t *testing.T) {
	_, store := newBaselineTestDB(t)

	_, err := store.LoadPinnedRole(context.Background(), "ghost", "0.0.0")
	if err == nil {
		t.Fatal("expected error for nonexistent role version")
	}
}

func TestBaselineStore_LoadPinnedSOP(t *testing.T) {
	repo, store := newBaselineTestDB(t)

	sop := &schemas.SOP{
		ID:             "deploy_sop",
		Version:        "1.0.0",
		Author:         "human_expert",
		MutableByAgent: false,
		Description:    "Deployment workflow",
		Steps:          map[string]schemas.SOPStep{},
	}
	if err := repo.SyncSOPsToDB(map[string]*schemas.SOP{"deploy_sop": sop}); err != nil {
		t.Fatalf("SyncSOPsToDB: %v", err)
	}

	pinned, err := store.LoadPinnedSOP(context.Background(), "deploy_sop", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPinnedSOP: %v", err)
	}
	if pinned.ID != "deploy_sop" || pinned.Version != "1.0.0" {
		t.Errorf("got %s/%s, want deploy_sop/1.0.0", pinned.ID, pinned.Version)
	}
}

func TestBaselineStore_UnversionedRoleSkipped(t *testing.T) {
	repo, store := newBaselineTestDB(t)

	role := &config.Role{ID: "legacy", Name: "Legacy", BaseCapability: "general"}
	if err := repo.SyncFileToDB(map[string]*config.Role{"legacy": role}); err != nil {
		t.Fatalf("SyncFileToDB: %v", err)
	}

	_, err := store.LoadPinnedRole(context.Background(), "legacy", "")
	if err == nil {
		t.Fatal("expected error for unversioned role")
	}
}

func TestBaselineStore_PinOverwrite(t *testing.T) {
	_, store := newBaselineTestDB(t)
	ctx := context.Background()

	store.PinBaseline(ctx, "task-x", "sop_a", "1.0", "role_a", "1.0")
	store.PinBaseline(ctx, "task-x", "sop_b", "2.0", "role_b", "2.0")

	b, err := store.GetBaseline(ctx, "task-x") // audit NEW-L5: check error before deref
	if err != nil {
		t.Fatalf("GetBaseline: %v", err)
	}
	if b == nil {
		t.Fatal("expected baseline after pin, got nil")
	}
	if b.SOPID != "sop_b" {
		t.Errorf("expected overwrite to sop_b, got %s", b.SOPID)
	}
}
