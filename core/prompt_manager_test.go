package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptManager_Render(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "prompts_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	promptContent := "Hello {{.name}}!"
	promptFile := filepath.Join(tmpDir, "hello.md")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}

	pm := NewPromptManager(tmpDir, nil)

	raw, err := pm.GetPrompt("hello.md")
	if err != nil {
		t.Fatalf("GetPrompt failed: %v", err)
	}

	context := map[string]string{"name": "World"}
	rendered, err := pm.Render("hello.md", raw, context)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	expected := "Hello World!"
	if rendered != expected {
		t.Errorf("expected %q, got %q", expected, rendered)
	}
}

func TestPromptManager_Cache(t *testing.T) {
	pm := NewPromptManager("", nil)

	prompt := "Count: {{.count}}"
	context1 := map[string]int{"count": 1}

	res1, err := pm.Render("test", prompt, context1)
	if err != nil {
		t.Fatalf("first render failed: %v", err)
	}
	if res1 != "Count: 1" {
		t.Errorf("expected Count: 1, got %s", res1)
	}

	// Verify it's cached by using a different template with the same ref (should still use old one)
	res2, err := pm.Render("test", "New: {{.count}}", context1)
	if err != nil {
		t.Fatalf("second render failed: %v", err)
	}
	if res2 != "Count: 1" {
		t.Errorf("expected cached Count: 1, got %s", res2)
	}

	pm.ClearCache()
	res3, err := pm.Render("test", "New: {{.count}}", context1)
	if err != nil {
		t.Fatalf("third render failed: %v", err)
	}
	if res3 != "New: 1" {
		t.Errorf("expected New: 1 after cache clear, got %s", res3)
	}
}

func TestPromptManager_CookbookTemplating(t *testing.T) {
	tmpDir := t.TempDir()

	cookbookContent := "Strategy: {{.strategy}}"
	cookbookPath := filepath.Join(tmpDir, "strategy.md")
	os.WriteFile(cookbookPath, []byte(cookbookContent), 0644)

	pm := NewPromptManager("", nil) // No promptDir needed for absolute paths in loader

	// ResourceLoader handles absolute paths
	context := map[string]string{"strategy": "Divide and Conquer"}
	rendered, err := pm.GetAndRender(cookbookPath, context)
	if err != nil {
		t.Fatalf("GetAndRender failed: %v", err)
	}

	expected := "Strategy: Divide and Conquer"
	if rendered != expected {
		t.Errorf("expected %q, got %q", expected, rendered)
	}
}

// TestBaseRoleRender_NoNoValue verifies that rendering base_role.md with a
// complete template context produces no "<no value>" artifacts.
//
// Defect: D-PROMPT-4 (§4.4) — base_role.md previously referenced {{.user_name}}
// which was never provided in tmplCtx, causing "<no value>" to appear in the
// rendered system prompt. The fix removes the {{.user_name}} reference and
// adds missingkey=error to the template parser.
//
// Before fix: render output contained "<no value>" (from {{.user_name}} with
// default missingkey=invalid). After fix: all keys are provided, no "<no value>".
//
// See: docs/plan/ASSET_GC_AND_DELEGATION_R2_REMEDIATION.md §6
func TestBaseRoleRender_NoNoValue(t *testing.T) {
	pm := NewPromptManager("../prompts", nil)

	tmplCtx := map[string]any{
		"task": map[string]any{
			"id":   "test-task-001",
			"step": "step-1",
			"role": "coder",
		},
		"env": map[string]string{
			"Time": "2026-11-02T12:00:00Z",
		},
	}

	rendered, err := pm.GetAndRender("base_role.md", tmplCtx)
	if err != nil {
		t.Fatalf("GetAndRender failed: %v", err)
	}

	if strings.Contains(rendered, "<no value>") {
		t.Errorf("rendered base_role.md contains <no value>:\n%s", rendered)
	}
}

// TestPromptManagerMissingKeyErrors verifies that the PromptManager's template
// parser uses missingkey=error, so a missing key produces an error instead of
// silently rendering "<no value>".
//
// Defect: D-PROMPT-4 (§4.4) — prompt_manager.go previously parsed templates
// without missingkey=error, so missing keys rendered as "<no value>" with no
// error. The fix adds .Option("missingkey=error") to the template parser.
//
// Before fix: Render succeeds, output contains "<no value>". After fix: Render
// returns an error.
//
// See: docs/plan/ASSET_GC_AND_DELEGATION_R2_REMEDIATION.md §6
func TestPromptManagerMissingKeyErrors(t *testing.T) {
	pm := NewPromptManager("", nil)

	ctx := map[string]any{
		"known": "value",
	}

	_, err := pm.Render("test_missing_key", "Hello {{.known}} {{.nonexistent}}", ctx)
	if err == nil {
		t.Fatal("expected error for missing key with missingkey=error, got nil")
	}

	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the missing key, got: %v", err)
	}
}
