package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestPromptAssembler_PromptManagerFallback_RoleInstructionEmpty verifies that
// when a role has no custom Instruction, the PromptManager loads base_role.md
// from the prompts/ dir and renders it as the role instruction block.
// Regression for 2026-10-18 PromptManager wiring (ONION_ARCHITECTURE.md §8).
func TestPromptAssembler_PromptManagerFallback_RoleInstructionEmpty(t *testing.T) {
	promptDir := t.TempDir()
	templateContent := "# Base AI Role\nHello {{.task.role}}!\nTask ID: {{.task.id}}\nTime: {{index .env \"Time\"}}\n"
	if err := os.WriteFile(filepath.Join(promptDir, "base_role.md"), []byte(templateContent), 0644); err != nil {
		t.Fatalf("failed to write template: %v", err)
	}

	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.promptManager = NewPromptManager(promptDir, nil)

	role := &config.Role{
		ID:          "test-role",
		Name:        "TestRole",
		Instruction: "",
	}

	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-123",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Base AI Role") {
			found = true
			if !strings.Contains(b.Text, "Hello TestRole!") {
				t.Errorf("expected rendered role name 'TestRole', got: %s", b.Text)
			}
			if !strings.Contains(b.Text, "Task ID: task-123") {
				t.Errorf("expected rendered task ID 'task-123', got: %s", b.Text)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected base_role.md fallback block, not found")
	}
}

// TestPromptAssembler_PromptManagerFallback_SkippedWhenInstructionPresent
// verifies that when a role has a custom Instruction, the PromptManager
// fallback is NOT used (existing behavior preserved).
func TestPromptAssembler_PromptManagerFallback_SkippedWhenInstructionPresent(t *testing.T) {
	promptDir := t.TempDir()
	templateContent := "# Base AI Role SHOULD NOT APPEAR\n"
	os.WriteFile(filepath.Join(promptDir, "base_role.md"), []byte(templateContent), 0644)

	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.promptManager = NewPromptManager(promptDir, nil)

	role := &config.Role{
		ID:          "test-role",
		Name:        "TestRole",
		Instruction: "You are a custom agent.",
	}

	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-456",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, b := range blocks {
		if strings.Contains(b.Text, "SHOULD NOT APPEAR") {
			t.Fatal("fallback template was used despite non-empty role.Instruction")
		}
	}
}

// TestPromptAssembler_PromptManagerFallback_NilSafe verifies that a nil
// promptManager does not cause a panic when role.Instruction is empty.
func TestPromptAssembler_PromptManagerFallback_NilSafe(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.promptManager = nil

	role := &config.Role{
		ID:          "test-role",
		Name:        "TestRole",
		Instruction: "",
	}

	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-789",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	for _, b := range blocks {
		if strings.Contains(b.Text, "Base AI Role") {
			t.Fatal("expected no fallback block with nil promptManager")
		}
	}
}
