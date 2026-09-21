package core

import (
	"os"
	"path/filepath"
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
	tmpDir, _ := os.MkdirTemp("", "cookbook_test")
	defer os.RemoveAll(tmpDir)

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
