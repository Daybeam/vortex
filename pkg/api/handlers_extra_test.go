package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

// --- Delete Role ---

func TestDeleteRoleHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	// Add a role first
	roleJSON, _ := json.Marshal(config.Role{ID: "del_me", Name: "Delete Me"})
	ops := []config.PatchOp{{Op: config.OpAdd, Path: "/roles/del_me", Value: json.RawMessage(roleJSON)}}
	if err := app.Registry.CommitOps(ops, "test", ""); err != nil {
		t.Fatal(err)
	}

	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{"id": "del_me"})
	req, _ := http.NewRequest("POST", "/api/config/delete-role", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteRoleHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	app.Registry.Mu.RLock()
	_, exists := app.Registry.Roles["del_me"]
	app.Registry.Mu.RUnlock()
	if exists {
		t.Error("role should have been deleted from registry")
	}
}

// --- Delete MCP ---

func TestDeleteMCPHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	mcpJSON, _ := json.Marshal(config.MCPDef{ID: "del_mcp", URL: "http://localhost:9999/sse"})
	ops := []config.PatchOp{{Op: config.OpAdd, Path: "/mcps/del_mcp", Value: json.RawMessage(mcpJSON)}}
	if err := app.Registry.CommitOps(ops, "test", ""); err != nil {
		t.Fatal(err)
	}

	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{"id": "del_mcp"})
	req, _ := http.NewRequest("POST", "/api/config/delete-mcp", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteMCPHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	app.Registry.Mu.RLock()
	_, exists := app.Registry.MCPs["del_mcp"]
	app.Registry.Mu.RUnlock()
	if exists {
		t.Error("MCP should have been deleted from registry")
	}
}

// --- Delete Provider ---

func TestDeleteProviderHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	// "test" provider already exists from setupTestApp
	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{"name": "test"})
	req, _ := http.NewRequest("POST", "/api/config/delete-provider", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteProviderHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	app.Registry.Mu.RLock()
	_, exists := app.Registry.Providers["test"]
	app.Registry.Mu.RUnlock()
	if exists {
		t.Error("provider should have been deleted from registry")
	}
}

// --- Delete Group ---

func TestDeleteGroupHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	groupJSON, _ := json.Marshal(config.RoleGroup{ID: "del_grp", Name: "Del Group", Policy: "fallback"})
	ops := []config.PatchOp{{Op: config.OpAdd, Path: "/role_groups/del_grp", Value: json.RawMessage(groupJSON)}}
	if err := app.Registry.CommitOps(ops, "test", ""); err != nil {
		t.Fatal(err)
	}

	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{"id": "del_grp"})
	req, _ := http.NewRequest("POST", "/api/config/delete-group", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteGroupHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	app.Registry.Mu.RLock()
	_, exists := app.Registry.RoleGroups["del_grp"]
	app.Registry.Mu.RUnlock()
	if exists {
		t.Error("group should have been deleted from registry")
	}
}

// --- Delete Skill (file-based) ---

func TestDeleteSkillHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	// Create skill file + registry entry
	skillsDir := filepath.Join(filepath.Dir(app.Registry.GetConfigPath()), "workspace", "skills")
	os.MkdirAll(skillsDir, 0755)
	skillPath := filepath.Join(skillsDir, "del_skill.json")
	os.WriteFile(skillPath, []byte(`{"id":"del_skill","name":"Del"}`), 0644)

	app.Registry.Mu.Lock()
	app.Registry.Skills["del_skill"] = &config.Skill{ID: "del_skill", Name: "Del"}
	app.Registry.Mu.Unlock()

	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{"id": "del_skill"})
	req, _ := http.NewRequest("POST", "/api/config/delete-skill", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteSkillHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// File should be gone
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Error("skill file should have been deleted")
	}

	// Map entry should be gone
	app.Registry.Mu.RLock()
	_, exists := app.Registry.Skills["del_skill"]
	app.Registry.Mu.RUnlock()
	if exists {
		t.Error("skill should have been deleted from registry map")
	}
}

// --- Delete SOP (file-based) ---

func TestDeleteSOPHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	// Create SOP file + registry entry
	sopsDir := filepath.Join(filepath.Dir(app.Registry.GetConfigPath()), "workspace", "sops")
	os.MkdirAll(sopsDir, 0755)
	sopPath := filepath.Join(sopsDir, "del_sop.json")
	os.WriteFile(sopPath, []byte(`{"id":"del_sop","description":"Del"}`), 0644)

	app.Registry.Mu.Lock()
	app.Registry.SOPs["del_sop"] = &schemas.SOP{ID: "del_sop", Description: "Del"}
	app.Registry.Mu.Unlock()

	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{"id": "del_sop"})
	req, _ := http.NewRequest("POST", "/api/config/delete-sop", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteSOPHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// File should be gone
	if _, err := os.Stat(sopPath); !os.IsNotExist(err) {
		t.Error("SOP file should have been deleted")
	}

	// Map entry should be gone
	app.Registry.Mu.RLock()
	_, exists := app.Registry.SOPs["del_sop"]
	app.Registry.Mu.RUnlock()
	if exists {
		t.Error("SOP should have been deleted from registry map")
	}
}

// --- Delete with missing ID should 400 ---

func TestDeleteRoleHandler_MissingID(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	handler := NewAPIHandler(app)
	body, _ := json.Marshal(map[string]string{})
	req, _ := http.NewRequest("POST", "/api/config/delete-role", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.DeleteRoleHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// --- Delete with wrong method should 405 ---

func TestDeleteRoleHandler_WrongMethod(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	handler := NewAPIHandler(app)
	req, _ := http.NewRequest("GET", "/api/config/delete-role", nil)
	rr := httptest.NewRecorder()
	handler.DeleteRoleHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// TestSubmitSteps_NilSchedulerReturnsError is a regression test for audit L-15-1/L-15-2:
// submitSteps must return a non-nil error (not silently succeed) when the handler
// returns a non-200 status. With a nil scheduler, TaskSubmitHandler returns 503.
//
// Before L-15-2 fix: if http.NewRequest failed, req was nil → panic.
// Before L-15-1 fix: if response lacked task_id, returned ("", nil) — silent success.
// After fix: error is properly propagated to the caller.
func TestSubmitSteps_NilSchedulerReturnsError(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)
	// app.Scheduler is nil — TaskSubmitHandler returns 503.

	handler := NewAPIHandler(app)
	steps := []map[string]any{
		{"id": "s1", "task": "test"},
	}
	taskID, err := handler.submitSteps(steps)
	if err == nil {
		t.Fatalf("expected error for nil scheduler, got taskID=%q err=nil (audit L-15-1: error was silently ignored)", taskID)
	}
	if taskID != "" {
		t.Errorf("expected empty taskID on error, got %q", taskID)
	}
}

// TestSubmitSteps_ValidReturnsNonEmptyID is a regression test for audit L-15-1:
// verifies the happy path still works after adding the json.Unmarshal error check
// and non-empty TaskID validation. A working scheduler returns a real UUID.
func TestSubmitSteps_ValidReturnsNonEmptyID(t *testing.T) {
	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	tmpDir := t.TempDir()
	engine := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, tmpDir, tmpDir, nil)
	t.Cleanup(func() { engine.Stop() })

	app := &tools.App{Scheduler: engine, Registry: reg}
	handler := NewAPIHandler(app)

	steps := []map[string]any{
		{"id": "s1", "task": "test task", "role": "default"},
	}
	taskID, err := handler.submitSteps(steps)
	if err != nil {
		t.Fatalf("submitSteps failed on valid input: %v", err)
	}
	if taskID == "" {
		t.Fatal("expected non-empty taskID, got empty string (audit L-15-1: non-empty check should pass for valid scheduler)")
	}
}

// TestUpdateMCPHandler_StdioTransport is a regression test for B-28-2:
// UpdateMCPHandler rejected stdio MCPs by requiring URL != "". Now it accepts
// either URL or Command (at least one transport must be specified).
//
// Bug: B-28-2 (2026-10-28) — UpdateMCPHandler required URL, blocking stdio.
func TestUpdateMCPHandler_StdioTransport(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	handler := NewAPIHandler(app)
	mcpBody, _ := json.Marshal(config.MCPDef{
		ID:      "test-stdio-mcp",
		Command: "python3",
		Args:    []string{"-m", "server"},
		Trusted: true,
	})
	req, _ := http.NewRequest("POST", "/api/config/update-mcp", bytes.NewBuffer(mcpBody))
	rr := httptest.NewRecorder()
	handler.UpdateMCPHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("B-28-2: expected 200 for stdio MCP, got %d: %s", rr.Code, rr.Body.String())
	}

	app.Registry.Mu.RLock()
	mcp, exists := app.Registry.MCPs["test-stdio-mcp"]
	app.Registry.Mu.RUnlock()
	if !exists {
		t.Fatal("B-28-2: stdio MCP not registered in Registry.MCPs")
	}
	if mcp.Command != "python3" {
		t.Errorf("B-28-2: expected Command='python3', got %q", mcp.Command)
	}
}

// TestUpdateMCPHandler_RejectsNoTransport verifies that an MCP with neither
// URL nor Command is still rejected (the validation wasn't removed, just relaxed).
func TestUpdateMCPHandler_RejectsNoTransport(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	handler := NewAPIHandler(app)
	mcpBody, _ := json.Marshal(config.MCPDef{ID: "no-transport-mcp"})
	req, _ := http.NewRequest("POST", "/api/config/update-mcp", bytes.NewBuffer(mcpBody))
	rr := httptest.NewRecorder()
	handler.UpdateMCPHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for MCP with no transport, got %d", rr.Code)
	}
}
