package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestEvaluateAndGuardInputs_NormalTask(t *testing.T) {
	inputs := []schemas.StepInput{
		{ID: "s1", Task: "Short task instruction under 5000 chars", RoleID: "coder"},
	}
	reg := &config.Registry{
		Roles: map[string]*config.Role{"coder": {}},
		MCPs:  map[string]*config.MCPDef{"leann-mcp": {}},
	}

	err := EvaluateAndGuardInputs(inputs, reg)
	if err != nil {
		t.Fatalf("expected no error for normal task, got: %v", err)
	}
}

func TestEvaluateAndGuardInputs_AssetRefBypassesGuard(t *testing.T) {
	longTask := "asset://some/large/document/path.md " + strings.Repeat("a", 6000)
	inputs := []schemas.StepInput{
		{ID: "s1", Task: longTask, RoleID: "analyst"},
	}
	reg := &config.Registry{
		Roles: map[string]*config.Role{"analyst": {}},
	}

	err := EvaluateAndGuardInputs(inputs, reg)
	if err != nil {
		t.Fatalf("expected asset:// prefix to bypass guard, got: %v", err)
	}
}

func TestEvaluateAndGuardInputs_PayloadExceededTriggersGuard(t *testing.T) {
	longTask := strings.Repeat("x", 5001)
	inputs := []schemas.StepInput{
		{ID: "s1", Task: longTask, RoleID: "analyst"},
	}
	reg := &config.Registry{
		Roles: map[string]*config.Role{"book_researcher": {}, "analyst": {}},
		MCPs:  map[string]*config.MCPDef{"leann-mcp": {}, "doc-parser": {}},
	}

	err := EvaluateAndGuardInputs(inputs, reg)
	if err == nil {
		t.Fatal("expected GuardViolationError for task > 5000 chars, got nil")
	}

	guardErr, ok := err.(*GuardViolationError)
	if !ok {
		t.Fatalf("expected *GuardViolationError, got %T", err)
	}

	if guardErr.ViolationType != "PAYLOAD_SIZE_EXCEEDED" {
		t.Fatalf("expected violation type PAYLOAD_SIZE_EXCEEDED, got %s", guardErr.ViolationType)
	}

	if guardErr.CurrentLength <= 5000 {
		t.Fatalf("expected current length > 5000, got %d", guardErr.CurrentLength)
	}

	// Verify dynamic capabilities are reflected
	foundAnalyst := false
	for _, r := range guardErr.AvailableRoles {
		if r == "analyst" {
			foundAnalyst = true
		}
	}
	if !foundAnalyst {
		t.Fatal("expected 'analyst' in available roles hint")
	}
}

func TestEvaluateAndGuardInputs_NilRegistryFallback(t *testing.T) {
	longTask := strings.Repeat("y", 5001)
	inputs := []schemas.StepInput{
		{ID: "s1", Task: longTask},
	}

	err := EvaluateAndGuardInputs(inputs, nil)
	if err == nil {
		t.Fatal("expected error with nil registry fallback")
	}

	guardErr := err.(*GuardViolationError)
	if len(guardErr.AvailableRoles) == 0 || len(guardErr.AvailableMCPs) == 0 {
		t.Fatal("expected fallback default roles and MCPs when registry is nil")
	}
}
