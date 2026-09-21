package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// helper: create a fresh in-memory SQLite + repo for each test
func newTestRepo(t *testing.T) (*SQLiteRepo, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return NewSQLiteRepo(db), tmpDir
}

func TestSaveRoleAndPersistFile(t *testing.T) {
	repo, tmpDir := newTestRepo(t)
	rolePath := filepath.Join(tmpDir, "analyst.json")

	role := &config.Role{
		ID:             "analyst",
		Name:           "Data Analyst",
		BaseCapability: "analyze",
		Provider:       "sensenova",
		Model:          "sensenova-6.7-flash-lite",
		BoundSkills:    []string{"skill_fetch_market_data"},
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "invest-research-lite", AllowedTools: []string{}},
		},
		AllowDynamicMCPs: true,
	}

	// Act
	err := repo.SaveRoleAndPersistFile(role, rolePath)
	if err != nil {
		t.Fatalf("SaveRoleAndPersistFile failed: %v", err)
	}

	// Assert 1: JSON file was written
	fileData, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatalf("failed to read JSON file: %v", err)
	}
	var fileRole config.Role
	if err := json.Unmarshal(fileData, &fileRole); err != nil {
		t.Fatalf("failed to unmarshal JSON file: %v", err)
	}
	if fileRole.ID != role.ID || fileRole.Name != role.Name {
		t.Errorf("JSON file role mismatch: got %+v, want ID=%s Name=%s", fileRole, role.ID, role.Name)
	}
	if fileRole.Provider != "sensenova" || fileRole.Model != "sensenova-6.7-flash-lite" {
		t.Errorf("JSON file provider/model mismatch: got %s/%s", fileRole.Provider, fileRole.Model)
	}
	if len(fileRole.BoundMCPBindings) != 1 || fileRole.BoundMCPBindings[0].MCPID != "invest-research-lite" {
		t.Errorf("JSON file MCP bindings mismatch: got %v", fileRole.BoundMCPBindings)
	}
	if !fileRole.AllowDynamicMCPs {
		t.Error("JSON file AllowDynamicMCPs should be true")
	}

	// Assert 2: DB row was written
	var dbRaw string
	err = repo.db.QueryRow(`SELECT raw_json FROM roles_meta WHERE id = ?`, role.ID).Scan(&dbRaw)
	if err != nil {
		t.Fatalf("failed to query DB for role: %v", err)
	}
	var dbRole config.Role
	if err := json.Unmarshal([]byte(dbRaw), &dbRole); err != nil {
		t.Fatalf("failed to unmarshal DB raw_json: %v", err)
	}
	if dbRole.ID != role.ID || dbRole.Name != role.Name {
		t.Errorf("DB role mismatch: got %+v", dbRole)
	}
}

func TestSaveRoleUpdateExisting(t *testing.T) {
	repo, tmpDir := newTestRepo(t)
	rolePath := filepath.Join(tmpDir, "researcher.json")

	// Create initial role
	role1 := &config.Role{
		ID:             "researcher",
		Name:           "Initial Researcher",
		BaseCapability: "analyze",
		Provider:       "gemini",
		Model:          "gemini-flash-lite",
	}
	if err := repo.SaveRoleAndPersistFile(role1, rolePath); err != nil {
		t.Fatalf("first save failed: %v", err)
	}

	// Update with new provider/model
	role2 := &config.Role{
		ID:               "researcher",
		Name:             "Updated Researcher",
		BaseCapability:   "analyze",
		Provider:         "deepseek-v4-flash",
		Model:            "deepseek-v4-flash",
		AllowDynamicMCPs: true,
	}
	if err := repo.SaveRoleAndPersistFile(role2, rolePath); err != nil {
		t.Fatalf("second save failed: %v", err)
	}

	// Assert: DB should have the updated version (ON CONFLICT DO UPDATE)
	var dbRaw string
	err := repo.db.QueryRow(`SELECT raw_json FROM roles_meta WHERE id = ?`, "researcher").Scan(&dbRaw)
	if err != nil {
		t.Fatalf("failed to query DB: %v", err)
	}
	var dbRole config.Role
	json.Unmarshal([]byte(dbRaw), &dbRole)
	if dbRole.Name != "Updated Researcher" {
		t.Errorf("DB name not updated: got %s", dbRole.Name)
	}
	if dbRole.Provider != "deepseek-v4-flash" {
		t.Errorf("DB provider not updated: got %s", dbRole.Provider)
	}
	if !dbRole.AllowDynamicMCPs {
		t.Error("DB AllowDynamicMCPs should be true after update")
	}

	// Assert: JSON file should also be updated
	fileData, _ := os.ReadFile(rolePath)
	var fileRole config.Role
	json.Unmarshal(fileData, &fileRole)
	if fileRole.Name != "Updated Researcher" {
		t.Errorf("JSON file name not updated: got %s", fileRole.Name)
	}

	// Assert: only 1 row in DB (not 2)
	var count int
	repo.db.QueryRow(`SELECT COUNT(*) FROM roles_meta WHERE id = ?`, "researcher").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 DB row, got %d", count)
	}
}

func TestSyncFileToDB(t *testing.T) {
	repo, _ := newTestRepo(t)

	roles := map[string]*config.Role{
		"analyst": {
			ID:             "analyst",
			Name:           "Analyst",
			BaseCapability: "analyze",
			Provider:       "sensenova",
			Model:          "sensenova-6.7-flash-lite",
		},
		"coder": {
			ID:             "coder",
			Name:           "Coder",
			BaseCapability: "code",
			Provider:       "deepseek-v4-flash",
			Model:          "deepseek-v4-flash",
		},
	}

	err := repo.SyncFileToDB(roles)
	if err != nil {
		t.Fatalf("SyncFileToDB failed: %v", err)
	}

	// Assert both roles are in DB
	for id, expected := range roles {
		var dbRaw string
		err := repo.db.QueryRow(`SELECT raw_json FROM roles_meta WHERE id = ?`, id).Scan(&dbRaw)
		if err != nil {
			t.Errorf("role %q not found in DB: %v", id, err)
			continue
		}
		var dbRole config.Role
		json.Unmarshal([]byte(dbRaw), &dbRole)
		if dbRole.Name != expected.Name {
			t.Errorf("role %q: DB name mismatch: got %s, want %s", id, dbRole.Name, expected.Name)
		}
	}

	// Assert row count
	var count int
	repo.db.QueryRow(`SELECT COUNT(*) FROM roles_meta`).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 DB rows, got %d", count)
	}
}

func TestLoadRolesFromDB(t *testing.T) {
	repo, _ := newTestRepo(t)

	// Seed DB with roles
	roles := map[string]*config.Role{
		"analyst": {
			ID:             "analyst",
			Name:           "DB Analyst",
			BaseCapability: "analyze",
			Provider:       "sensenova",
			Model:          "sensenova-6.7-flash-lite",
			BoundSkills:    []string{"skill_a", "skill_b"},
		},
		"writer": {
			ID:             "writer",
			Name:           "DB Writer",
			BaseCapability: "write",
			Provider:       "glm-52",
			Model:          "glm-5.2",
		},
	}
	repo.SyncFileToDB(roles)

	// Act
	loaded, err := repo.LoadRolesFromDB()
	if err != nil {
		t.Fatalf("LoadRolesFromDB failed: %v", err)
	}

	// Assert
	if len(loaded) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(loaded))
	}
	for id, expected := range roles {
		got, ok := loaded[id]
		if !ok {
			t.Errorf("role %q missing from loaded result", id)
			continue
		}
		if got.Name != expected.Name {
			t.Errorf("role %q: name mismatch: got %s, want %s", id, got.Name, expected.Name)
		}
		if got.Provider != expected.Provider {
			t.Errorf("role %q: provider mismatch: got %s, want %s", id, got.Provider, expected.Provider)
		}
		if len(got.BoundSkills) != len(expected.BoundSkills) {
			t.Errorf("role %q: BoundSkills length mismatch: got %d, want %d", id, len(got.BoundSkills), len(expected.BoundSkills))
		}
	}
}

func TestLoadRolesFromDBEmpty(t *testing.T) {
	repo, _ := newTestRepo(t)

	loaded, err := repo.LoadRolesFromDB()
	if err != nil {
		t.Fatalf("LoadRolesFromDB on empty DB failed: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected 0 roles from empty DB, got %d", len(loaded))
	}
}

func TestDeleteRole(t *testing.T) {
	repo, tmpDir := newTestRepo(t)
	rolePath := filepath.Join(tmpDir, "deleteme.json")

	// Create a role
	role := &config.Role{
		ID:             "deleteme",
		Name:           "Delete Me",
		BaseCapability: "analyze",
	}
	repo.SaveRoleAndPersistFile(role, rolePath)

	// Verify it exists
	var count int
	repo.db.QueryRow(`SELECT COUNT(*) FROM roles_meta WHERE id = ?`, "deleteme").Scan(&count)
	if count != 1 {
		t.Fatalf("role not in DB before delete")
	}
	if _, err := os.Stat(rolePath); err != nil {
		t.Fatalf("JSON file not found before delete")
	}

	// Act
	err := repo.DeleteRole("deleteme", rolePath)
	if err != nil {
		t.Fatalf("DeleteRole failed: %v", err)
	}

	// Assert DB row deleted
	repo.db.QueryRow(`SELECT COUNT(*) FROM roles_meta WHERE id = ?`, "deleteme").Scan(&count)
	if count != 0 {
		t.Errorf("role still in DB after delete: count=%d", count)
	}

	// Assert JSON file deleted
	if _, err := os.Stat(rolePath); !os.IsNotExist(err) {
		t.Errorf("JSON file still exists after delete")
	}
}

func TestDeleteRoleNoFile(t *testing.T) {
	repo, tmpDir := newTestRepo(t)
	rolePath := filepath.Join(tmpDir, "nofile.json")

	role := &config.Role{
		ID:             "nofile",
		Name:           "No File",
		BaseCapability: "analyze",
	}
	repo.SaveRoleAndPersistFile(role, rolePath)

	// Manually delete the JSON file before calling DeleteRole
	os.Remove(rolePath)

	// Act — should still succeed (file already gone)
	err := repo.DeleteRole("nofile", rolePath)
	if err != nil {
		t.Fatalf("DeleteRole should succeed even if file is gone: %v", err)
	}

	// Assert DB row deleted
	var count int
	repo.db.QueryRow(`SELECT COUNT(*) FROM roles_meta WHERE id = ?`, "nofile").Scan(&count)
	if count != 0 {
		t.Errorf("role still in DB after delete")
	}
}

func TestDeleteRoleEmptyPath(t *testing.T) {
	repo, _ := newTestRepo(t)

	role := &config.Role{
		ID:             "nojson",
		Name:           "No JSON File",
		BaseCapability: "analyze",
	}
	// Save to DB only (path is empty → WriteFile will fail silently in SaveRoleAndPersistFile)
	// We'll insert directly
	repo.SyncFileToDB(map[string]*config.Role{"nojson": role})

	// Act — empty path should just skip file deletion
	err := repo.DeleteRole("nojson", "")
	if err != nil {
		t.Fatalf("DeleteRole with empty path failed: %v", err)
	}

	var count int
	repo.db.QueryRow(`SELECT COUNT(*) FROM roles_meta WHERE id = ?`, "nojson").Scan(&count)
	if count != 0 {
		t.Errorf("role still in DB after delete with empty path")
	}
}

func TestDBWALMode(t *testing.T) {
	// Verify WAL mode is actually enabled
	repo, _ := newTestRepo(t)

	var journalMode string
	err := repo.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode)
	if err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode=wal, got %s", journalMode)
	}
}

func TestSchemaMigrationsIdempotent(t *testing.T) {
	// Running InitDB twice on same DB should not fail
	repo, tmpDir := newTestRepo(t)
	dbPath := filepath.Join(tmpDir, "test.db")

	// Close first connection
	repo.db.Close()

	// Reopen — migrations should be idempotent (CREATE TABLE IF NOT EXISTS)
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("second InitDB failed: %v", err)
	}
	defer db.Close()

	// Verify tables still exist
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM roles_meta`).Scan(&count)
	if err != nil {
		t.Fatalf("roles_meta table not found after reopen: %v", err)
	}
}

func TestSaveRoleWithComplexFields(t *testing.T) {
	repo, tmpDir := newTestRepo(t)
	rolePath := filepath.Join(tmpDir, "complex.json")

	role := &config.Role{
		ID:             "complex_role",
		Name:           "Complex Role",
		BaseCapability: "analyze",
		Provider:       "sensenova",
		Model:          "sensenova-6.7-flash-lite",
		BoundSkills:    []string{"skill_a", "skill_b", "skill_c"},
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "invest-research-lite", AllowedTools: []string{"get_quote", "get_financials"}},
			{MCPID: "lsmcp", AllowedTools: []string{}},
		},
		AllowDynamicSkills:  true,
		AllowDynamicMCPs:    true,
		MaxAdditionalSkills: 5,
		Fallbacks: []config.RoleFallback{
			{Provider: "sensenova-flash-lite", Model: "sensenova-6.8-flash-lite"},
			{Provider: "deepseek-v4-flash", Model: "deepseek-v4-flash"},
		},
		DisableFallback: false,
		Purpose:         "Market research and analysis",
		BestFor:         "Stock analysis, financial research",
		Metadata:        map[string]string{"team": "research", "priority": "high"},
	}

	err := repo.SaveRoleAndPersistFile(role, rolePath)
	if err != nil {
		t.Fatalf("SaveRoleAndPersistFile failed for complex role: %v", err)
	}

	// Verify round-trip from DB
	var dbRaw string
	repo.db.QueryRow(`SELECT raw_json FROM roles_meta WHERE id = ?`, role.ID).Scan(&dbRaw)
	var dbRole config.Role
	json.Unmarshal([]byte(dbRaw), &dbRole)

	if len(dbRole.BoundMCPBindings) != 2 {
		t.Errorf("BoundMCPBindings: expected 2, got %d", len(dbRole.BoundMCPBindings))
	}
	if dbRole.BoundMCPBindings[0].MCPID != "invest-research-lite" {
		t.Errorf("BoundMCPBindings[0].MCPID mismatch: got %s", dbRole.BoundMCPBindings[0].MCPID)
	}
	if len(dbRole.BoundMCPBindings[0].AllowedTools) != 2 {
		t.Errorf("AllowedTools length mismatch: got %d", len(dbRole.BoundMCPBindings[0].AllowedTools))
	}
	if len(dbRole.Fallbacks) != 2 {
		t.Errorf("Fallbacks: expected 2, got %d", len(dbRole.Fallbacks))
	}
	if dbRole.Fallbacks[0].Model != "sensenova-6.8-flash-lite" {
		t.Errorf("Fallbacks[0].Model mismatch: got %s", dbRole.Fallbacks[0].Model)
	}
	if dbRole.Purpose != "Market research and analysis" {
		t.Errorf("Purpose mismatch: got %s", dbRole.Purpose)
	}
	if dbRole.Metadata["team"] != "research" {
		t.Errorf("Metadata[team] mismatch: got %s", dbRole.Metadata["team"])
	}
	if dbRole.MaxAdditionalSkills != 5 {
		t.Errorf("MaxAdditionalSkills mismatch: got %d", dbRole.MaxAdditionalSkills)
	}

	// Verify round-trip from JSON file
	fileData, _ := os.ReadFile(rolePath)
	var fileRole config.Role
	json.Unmarshal(fileData, &fileRole)
	if fileRole.Purpose != role.Purpose {
		t.Errorf("JSON Purpose mismatch: got %s, want %s", fileRole.Purpose, role.Purpose)
	}
	if len(fileRole.Fallbacks) != 2 {
		t.Errorf("JSON Fallbacks length mismatch: got %d", len(fileRole.Fallbacks))
	}
}
