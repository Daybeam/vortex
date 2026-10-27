package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPromptOverrides_MissingDirAllEmpty confirms that a fresh registry
// with no workspace/prompts/ directory at all behaves identically to before
// this feature existed: every PromptOverrides field is the empty string,
// and Load() does not error just because the directory is absent (mirrors
// how a missing workspace/skills/ or workspace/sops/ directory is treated
// as "nothing to load", not a failure).
func TestLoadPromptOverrides_MissingDirAllEmpty(t *testing.T) {
	// T-27-1: reset the process-wide AllowExternalAssets sync.Once cache so
	// this test is independent of TestAllowExternalAssets_False poisoning it
	// to false under -shuffle. Without this, if False runs first and True
	// does not run between them, loadPromptOverrides returns empty.
	resetAssetGate()
	os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	r := newSplitTestRegistry(t)
	r.Mu.RLock()
	po := r.PromptOverrides
	r.Mu.RUnlock()
	if po.ServerInstructions != "" || po.ServerInstructionsPublic != "" || po.OutputContract != "" {
		t.Fatalf("expected all-empty PromptOverrides with no workspace/prompts/ dir, got %+v", po)
	}
}

// TestLoadPromptOverrides_FilesPresentAreLoadedAndTrimmed writes all three
// override files and confirms Load() picks them up (via a fresh
// NewRegistry, matching how a real process would see them on startup) and
// trims surrounding whitespace (so a trailing newline left by a text editor
// doesn't leak into the actual instructions text).
func TestLoadPromptOverrides_FilesPresentAreLoadedAndTrimmed(t *testing.T) {
	// T-27-1: reset the process-wide AllowExternalAssets sync.Once cache (see
	// TestLoadPromptOverrides_MissingDirAllEmpty for the full rationale).
	resetAssetGate()
	os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config_test.json")
	// A minimal-but-valid config file is required here: Load() takes an
	// early bootstrap-defaults return path when the main config file is
	// entirely missing (os.ReadFile error), which skips the whole rest of
	// Load()'s body -- including skills/SOPs/roles loading AND, now,
	// prompt-override loading. That early-return behavior is pre-existing
	// and intentional (matches newSplitTestRegistry's pattern elsewhere in
	// this package); writing an actual (even empty) JSON config here is what
	// lets Load() reach the directory-scanning section this test wants to
	// exercise, mirroring what a real deployment's config_windows.json
	// (which always exists) does in production.
	if err := os.WriteFile(cfgPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("write config_test.json: %v", err)
	}
	promptsDir := filepath.Join(dir, "workspace", "prompts")
	if err := os.MkdirAll(promptsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "server_instructions.txt"), []byte("  custom admin instructions\n"), 0644); err != nil {
		t.Fatalf("write server_instructions.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "server_instructions_public.txt"), []byte("custom public instructions"), 0644); err != nil {
		t.Fatalf("write server_instructions_public.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "output_contract.txt"), []byte("CONTRACT for {{CAPABILITY}}: {{RESULT_HINT}}\n"), 0644); err != nil {
		t.Fatalf("write output_contract.txt: %v", err)
	}

	r, err := NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.Mu.RLock()
	po := r.PromptOverrides
	r.Mu.RUnlock()

	if po.ServerInstructions != "custom admin instructions" {
		t.Errorf("ServerInstructions = %q, want trimmed %q", po.ServerInstructions, "custom admin instructions")
	}
	if po.ServerInstructionsPublic != "custom public instructions" {
		t.Errorf("ServerInstructionsPublic = %q, want %q", po.ServerInstructionsPublic, "custom public instructions")
	}
	if po.OutputContract != "CONTRACT for {{CAPABILITY}}: {{RESULT_HINT}}" {
		t.Errorf("OutputContract = %q, want trimmed template", po.OutputContract)
	}
}

// TestLoadPromptOverrides_PartialOverrideLeavesOthersEmpty confirms that
// overriding just one of the three files doesn't require the other two to
// exist -- each is read independently, matching the "you don't need to
// override all three" behavior documented in workspace/prompts/README.md.
func TestLoadPromptOverrides_PartialOverrideLeavesOthersEmpty(t *testing.T) {
	// T-27-1: reset the process-wide AllowExternalAssets sync.Once cache (see
	// TestLoadPromptOverrides_MissingDirAllEmpty for the full rationale).
	resetAssetGate()
	os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config_test.json")
	// See the comment in TestLoadPromptOverrides_FilesPresentAreLoadedAndTrimmed
	// for why an actual config file (even an empty JSON object) must exist.
	if err := os.WriteFile(cfgPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("write config_test.json: %v", err)
	}
	promptsDir := filepath.Join(dir, "workspace", "prompts")
	if err := os.MkdirAll(promptsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "output_contract.txt"), []byte("only this one"), 0644); err != nil {
		t.Fatalf("write output_contract.txt: %v", err)
	}

	r, err := NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.Mu.RLock()
	po := r.PromptOverrides
	r.Mu.RUnlock()

	if po.OutputContract != "only this one" {
		t.Errorf("OutputContract = %q, want %q", po.OutputContract, "only this one")
	}
	if po.ServerInstructions != "" {
		t.Errorf("ServerInstructions should stay empty when its file doesn't exist, got %q", po.ServerInstructions)
	}
	if po.ServerInstructionsPublic != "" {
		t.Errorf("ServerInstructionsPublic should stay empty when its file doesn't exist, got %q", po.ServerInstructionsPublic)
	}
}

// TestLoadPromptOverrides_IndependentOfAssetGateCache is the regression test
// for T-27-1: a flaky test caused by AllowExternalAssets() using sync.Once
// to cache a process-wide flag. TestAllowExternalAssets_False poisons the
// cache to false; if TestAllowExternalAssets_True does not run before the
// prompt-override tests (which happens under -shuffle), loadPromptOverrides
// returns empty and TestLoadPromptOverrides_FilesPresentAreLoadedAndTrimmed
// fails. This test explicitly reproduces the poisoning scenario and verifies
// that resetAssetGate() restores independence.
//
// Before the fix, the prompt_overrides tests did not call resetAssetGate(),
// so they were order-dependent. After the fix, each test resets the cache.
func TestLoadPromptOverrides_IndependentOfAssetGateCache(t *testing.T) {
	// Phase 1: poison the cache to false (simulates TestAllowExternalAssets_False
	// running first under -shuffle).
	resetAssetGate()
	os.Setenv("VORTEX_ALLOW_EXTERNAL_ASSETS", "false")
	if AllowExternalAssets() {
		t.Fatal("setup: expected AllowExternalAssets()=false after poisoning")
	}

	// Phase 2: without reset, loadPromptOverrides returns empty (the bug).
	// We verify this to document the gate behavior.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config_test.json")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	promptsDir := filepath.Join(dir, "workspace", "prompts")
	if err := os.MkdirAll(promptsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "server_instructions.txt"), []byte("admin override"), 0644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	// Cache is still false → loadPromptOverrides returns empty.
	po := loadPromptOverrides(cfgPath)
	if po.ServerInstructions != "" {
		t.Fatalf("with cache=false, loadPromptOverrides should return empty; got ServerInstructions=%q", po.ServerInstructions)
	}

	// Phase 3: reset the cache + unset env (the fix) → overrides now load.
	resetAssetGate()
	os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	po = loadPromptOverrides(cfgPath)
	if po.ServerInstructions != "admin override" {
		t.Fatalf("after resetAssetGate(), loadPromptOverrides should load the override; got ServerInstructions=%q", po.ServerInstructions)
	}

	// Cleanup: leave the cache in a clean state for subsequent tests.
	resetAssetGate()
	os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
}
