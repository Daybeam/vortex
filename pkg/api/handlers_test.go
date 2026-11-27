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
	"github.com/daybeam/vortex/tools"
)

func setupTestApp(t *testing.T) (*tools.App, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "api_test")
	if err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tmpDir, "config.json")
	initialConfig := []byte(`{"default_provider": "test", "providers": {"test": {"provider": "openai", "model": "gpt-4"}}}`)
	if err := os.WriteFile(configPath, initialConfig, 0644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	app := &tools.App{
		Registry:   reg,
		ConfigPath: configPath,
	}
	return app, tmpDir
}

func TestConfigHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	handler := NewAPIHandler(app)
	req, _ := http.NewRequest("GET", "/api/config", nil)
	rr := httptest.NewRecorder()

	handler.ConfigHandler(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var cfg map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&cfg); err != nil {
		t.Logf("Response body: %s", rr.Body.String())
		t.Fatal(err)
	}

	if cfg["default_provider"] != "test" {
		t.Logf("Full config: %+v", cfg)
		t.Errorf("expected default_provider test, got %v", cfg["default_provider"])
	}
}

func TestUpdateRoleHandler(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	handler := NewAPIHandler(app)
	newRole := config.Role{ID: "new_role", Name: "New Role"}
	body, _ := json.Marshal(newRole)
	req, _ := http.NewRequest("POST", "/api/config/update-role", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()

	handler.UpdateRoleHandler(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	app.Registry.Mu.RLock()
	role, ok := app.Registry.Roles["new_role"]
	app.Registry.Mu.RUnlock()

	if !ok || role.Name != "New Role" {
		t.Errorf("role was not updated correctly")
	}
}
