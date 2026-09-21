package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestResolveServerInstructions_NilRegistryFallsBackToDefault confirms this
// is safe to call even before a Registry exists (e.g. very early in
// startup, or in a test harness that doesn't construct one) -- it should
// never panic on a nil reg, and should return the hardcoded default.
func TestResolveServerInstructions_NilRegistryFallsBackToDefault(t *testing.T) {
	if got := ResolveServerInstructions(nil, false); got != ServerInstructions {
		t.Fatalf("nil registry, public=false: expected the default ServerInstructions constant")
	}
	if got := ResolveServerInstructions(nil, true); got != ServerInstructionsPublic {
		t.Fatalf("nil registry, public=true: expected the default ServerInstructionsPublic constant")
	}
}

// TestResolveServerInstructions_EmptyOverrideFallsBackToDefault confirms a
// real Registry with no workspace/prompts/ files (the state of every
// existing deployment until someone opts in) still returns the compiled-in
// defaults, not an empty string.
func TestResolveServerInstructions_EmptyOverrideFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	reg, err := config.NewRegistry(filepath.Join(dir, "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := ResolveServerInstructions(reg, false); got != ServerInstructions {
		t.Fatalf("expected default ServerInstructions when no override file exists")
	}
	if got := ResolveServerInstructions(reg, true); got != ServerInstructionsPublic {
		t.Fatalf("expected default ServerInstructionsPublic when no override file exists")
	}
}

// TestResolveServerInstructions_OverridePreferredAndTierIsolated confirms
// that (a) an override file, once present, takes priority over the
// hardcoded constant, and (b) the admin/public overrides are looked up
// independently -- setting one does not affect the other tier's resolved
// text.
func TestResolveServerInstructions_OverridePreferredAndTierIsolated(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config_test.json")
	// A real (even empty) config file must exist so config.Registry.Load()
	// reaches its directory-scanning body instead of taking the early
	// bootstrap-defaults return path -- see the matching comment in
	// config/prompt_overrides_test.go for the full explanation.
	if err := os.WriteFile(cfgPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("write config_test.json: %v", err)
	}
	promptsDir := filepath.Join(dir, "workspace", "prompts")
	if err := writeTestPromptFile(t, filepath.Join(promptsDir, "server_instructions.txt"), "ADMIN OVERRIDE"); err != nil {
		t.Fatalf("writeTestPromptFile: %v", err)
	}
	reg, err := config.NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := ResolveServerInstructions(reg, false); got != "ADMIN OVERRIDE" {
		t.Fatalf("public=false: got %q, want override text", got)
	}
	// Public tier's own override file was never created -- it must still
	// fall back to the compiled-in default, not accidentally pick up the
	// admin override or return empty.
	if got := ResolveServerInstructions(reg, true); got != ServerInstructionsPublic {
		t.Fatalf("public=true: expected default ServerInstructionsPublic (admin override must not leak across tiers), got %q", got)
	}
}

func writeTestPromptFile(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}
