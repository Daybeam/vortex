package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodeEntityVerifier_ExistingEntities(t *testing.T) {
	// Use the core/ directory itself — these entities definitely exist.
	v := &CodeEntityVerifier{}
	criteria := map[string]any{
		"entities":   []any{"Spawner", "DirectedEngine", "Sieve"},
		"search_dir": "..", // core package dir (was "../core" before the core/verify extraction)
	}
	passed, msg, err := v.Verify(context.Background(), "", criteria)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !passed {
		t.Errorf("expected pass for existing entities, got: %s", msg)
	}
}

func TestCodeEntityVerifier_MissingEntity(t *testing.T) {
	v := &CodeEntityVerifier{}
	criteria := map[string]any{
		"entities":   []any{"Spawner", "NonExistentFunctionXYZ123"},
		"search_dir": "..", // core package dir (was "../core" before the core/verify extraction)
	}
	passed, msg, err := v.Verify(context.Background(), "", criteria)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if passed {
		t.Error("expected fail for non-existent entity, got pass")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestCodeEntityVerifier_MissingEntitiesParam(t *testing.T) {
	v := &CodeEntityVerifier{}
	passed, msg, err := v.Verify(context.Background(), "", map[string]any{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if passed {
		t.Error("expected fail for missing entities param")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestCodeEntityVerifier_NoSearchDir(t *testing.T) {
	v := &CodeEntityVerifier{}
	passed, msg, err := v.Verify(context.Background(), "", map[string]any{
		"entities": []any{"Spawner"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if passed {
		t.Error("expected fail when no search directory")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestCodeEntityVerifier_UsesWorkdir(t *testing.T) {
	v := &CodeEntityVerifier{}
	// ".." = the core package dir (was "../core" before the core/verify extraction).
	passed, _, err := v.Verify(context.Background(), "..", map[string]any{
		"entities": []any{"Spawner"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !passed {
		t.Error("expected pass when using workdir as search dir")
	}
}

func TestCodeEntityVerifier_SkipsVendorAndGit(t *testing.T) {
	dir := t.TempDir()
	// Create a vendor dir with a .go file containing the entity.
	vendorDir := filepath.Join(dir, "vendor", "pkg")
	if err := os.MkdirAll(vendorDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(vendorDir, "v.go"), []byte("package pkg\nfunc TargetFunc() {}\n"), 0644)
	// Create the real source dir.
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(srcDir, "s.go"), []byte("package src\nfunc OtherFunc() {}\n"), 0644)

	v := &CodeEntityVerifier{}
	// TargetFunc only exists in vendor/ — should NOT be found.
	passed, msg, _ := v.Verify(context.Background(), dir, map[string]any{
		"entities": []any{"TargetFunc"},
	})
	if passed {
		t.Error("entity in vendor/ should not be found")
	}
	if msg == "" {
		t.Error("expected failure message")
	}
	// OtherFunc in src/ should be found.
	// audit T-NEW-2: check Verify error for diagnostic clarity.
	passed, _, vErr := v.Verify(context.Background(), dir, map[string]any{
		"entities": []any{"OtherFunc"},
	})
	if vErr != nil {
		t.Fatalf("Verify failed: %v", vErr)
	}
	if !passed {
		t.Error("entity in src/ should be found")
	}
}
