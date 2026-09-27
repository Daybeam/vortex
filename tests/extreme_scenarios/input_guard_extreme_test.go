package extreme_tests

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// TestExtreme_InputGuard_SuperLargeDocument verifies that a 10MB inline
// task input is rejected by the Input Guard with an actionable error
// message listing available roles and MCPs.
//
// Extreme scenario: User pastes an entire 10MB PDF content directly
// into a task description instead of using asset:// reference.
func TestExtreme_InputGuard_SuperLargeDocument(t *testing.T) {
	// Build a minimal registry with one provider and one role.
	// Model "gpt-4o" → flagship tier → GetMaxSystemPromptChars() = 12000*4 = 48000.
	reg := &config.Registry{
		DefaultProvider: "test-provider",
		Providers: map[string]*config.ProviderConfig{
			"test-provider": {
				Provider:         "openai",
				Model:            "gpt-4o",
				MaxContextWindow: 128000,
			},
		},
		Roles: map[string]*config.Role{
			"analyst": {ID: "analyst", Name: "analyst"},
		},
		MCPs: map[string]*config.MCPDef{
			"fragment-mcp": {ID: "fragment-mcp"},
		},
	}

	// 10MB inline content — simulates user pasting entire PDF
	hugeContent := strings.Repeat("A", 10*1024*1024)

	inputs := []schemas.StepInput{
		{ID: "s1", RoleID: "analyst", Task: hugeContent},
	}

	err := core.EvaluateAndGuardInputs(inputs, reg)
	if err == nil {
		t.Fatal("expected GuardViolationError for 10MB inline input, got nil")
	}

	gve, ok := err.(*core.GuardViolationError)
	if !ok {
		t.Fatalf("expected *GuardViolationError, got %T: %v", err, err)
	}

	if gve.CurrentLength != len(hugeContent) {
		t.Errorf("CurrentLength = %d, want %d", gve.CurrentLength, len(hugeContent))
	}
	if gve.AllowedThreshold <= 0 {
		t.Error("AllowedThreshold should be positive")
	}
	if len(gve.AvailableRoles) == 0 {
		t.Error("AvailableRoles should not be empty — user needs guidance")
	}
	if len(gve.AvailableMCPs) == 0 {
		t.Error("AvailableMCPs should not be empty — user needs guidance")
	}
}

// TestExtreme_InputGuard_AssetRefBypassesSuperLarge verifies that
// asset://-prefixed super-large inputs bypass the guard correctly.
//
// Extreme scenario: User correctly references a 500MB file via asset://
func TestExtreme_InputGuard_AssetRefBypassesSuperLarge(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test-provider",
		Providers: map[string]*config.ProviderConfig{
			"test-provider": {
				Provider:         "openai",
				Model:            "gpt-4o",
				MaxContextWindow: 128000,
			},
		},
	}

	// 500MB asset reference — just the path, not the content
	assetRef := "asset://task-123/step-456/huge_file_500mb.bin"

	inputs := []schemas.StepInput{
		{ID: "s1", RoleID: "analyst", Task: assetRef},
	}

	err := core.EvaluateAndGuardInputs(inputs, reg)
	if err != nil {
		t.Fatalf("asset:// reference should bypass guard, got error: %v", err)
	}
}

// TestExtreme_InputGuard_MultipleSuperLargeDocuments verifies that
// submitting 10 super-large documents inline all get rejected.
//
// Extreme scenario: User pastes 10 × 5MB research papers inline.
func TestExtreme_InputGuard_MultipleSuperLargeDocuments(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test-provider",
		Providers: map[string]*config.ProviderConfig{
			"test-provider": {
				Provider:         "openai",
				Model:            "gpt-4o",
				MaxContextWindow: 128000,
			},
		},
		Roles: map[string]*config.Role{
			"researcher": {ID: "researcher", Name: "researcher"},
		},
	}

	hugeContent := strings.Repeat("B", 5*1024*1024) // 5MB per document

	inputs := make([]schemas.StepInput, 10)
	for i := range inputs {
		inputs[i] = schemas.StepInput{
			ID:     "s" + string(rune('1'+i)),
			RoleID: "researcher",
			Task:   hugeContent,
		}
	}

	err := core.EvaluateAndGuardInputs(inputs, reg)
	if err == nil {
		t.Fatal("expected guard violation for 10× 5MB inline inputs, got nil")
	}
}
