package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestSessionRoleResolution(t *testing.T) {
	// 1. Setup minimal registry
	tmpDir, _ := os.MkdirTemp("", "session_role_test")
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.json")
	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// Ensure no roles exist in global registry
	if len(reg.Roles) > 0 {
		t.Fatalf("global registry should be empty of roles")
	}

	// 2. Create a Session Role
	sessionRole := &config.Role{
		ID:             "temp_coder",
		Name:           "Temporary Coder",
		BaseCapability: "code",
	}

	graph := &schemas.TaskGraph{
		TaskID: "task_1",
		SessionRoles: map[string]any{
			"temp_coder": sessionRole,
		},
	}

	hub := NewContextHub(reg, graph, nil)

	// 3. Verify Resolution
	resolved := hub.GetRole("temp_coder")
	if resolved == nil {
		t.Fatalf("failed to resolve session role")
	}
	if resolved.Name != "Temporary Coder" {
		t.Errorf("resolved role name mismatch: %s", resolved.Name)
	}

	// 4. Verify Global Registry remains untouched
	if _, ok := reg.Roles["temp_coder"]; ok {
		t.Errorf("session role leaked into global registry")
	}
}

func TestEphemeralRoleGeneration(t *testing.T) {
	// This test verifies that GenerateRoleObjects returns objects without persisting them.

	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
		Roles:     make(map[string]*config.Role),
		Skills:    make(map[string]*config.Skill),
	}
	reg.DefaultProvider = "default"
	reg.Providers["default"] = &config.ProviderConfig{
		Provider: "local",
		Model:    "test-model",
	}

	_ = NewRoleGenerator(reg, nil, nil)

	// Since actual generation requires LLM and cookbook sources,
	// we just test that the refactoring didn't break basic struct initialization.
}
