package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestLocalDirSource_Discover_SKILLMD(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "code-review")
	os.MkdirAll(skillDir, 0755)

	skillContent := "---\nname: code-review\ndescription: Review code for best practices\ncapability: code-review\n---\n\n# Code Review\n\nYou are a code reviewer."
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)

	src := NewLocalDirSource("test-local", DiscoveryKindSkill, tmpDir, false)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "ext_code_review" {
		t.Errorf("expected ID 'ext_code_review', got %q", items[0].ID)
	}
	if items[0].Name != "code-review" {
		t.Errorf("expected Name 'code-review', got %q", items[0].Name)
	}
	if items[0].Capability != "code-review" {
		t.Errorf("expected Capability 'code-review', got %q", items[0].Capability)
	}
	if items[0].SourceName != "test-local" {
		t.Errorf("expected SourceName 'test-local', got %q", items[0].SourceName)
	}
	if items[0].TrustLevel != TrustLevelTrusted {
		t.Errorf("expected TrustLevel %q, got %q", TrustLevelTrusted, items[0].TrustLevel)
	}
}

func TestLocalDirSource_Discover_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	src := NewLocalDirSource("test-empty", DiscoveryKindSkill, tmpDir, false)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestLocalDirSource_Discover_NonExistentDir(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistent := filepath.Join(tmpDir, "does-not-exist")

	src := NewLocalDirSource("test-missing", DiscoveryKindSkill, nonExistent, false)
	_, err := src.Discover(context.Background())
	if err == nil {
		t.Fatal("expected error for non-existent directory, got nil")
	}
}

func TestLocalDirSource_Discover_StrictValid(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "web-scraper")
	os.MkdirAll(skillDir, 0755)

	skillContent := "---\nname: web-scraper\ndescription: Scrape web pages for content\n---\n\n# Web Scraper"
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)

	src := NewLocalDirSource("test-strict", DiscoveryKindSkill, tmpDir, true)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "web-scraper" {
		t.Errorf("expected Name 'web-scraper', got %q", items[0].Name)
	}
}

func TestLocalDirSource_Discover_StrictInvalidName(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "BadName")
	os.MkdirAll(skillDir, 0755)

	skillContent := "---\nname: BadName\ndescription: A skill with invalid name\n---\n\n# BadName"
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)

	src := NewLocalDirSource("test-strict-bad", DiscoveryKindSkill, tmpDir, true)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items (strict validation rejects BadName), got %d", len(items))
	}
}

func TestLocalDirSource_Discover_StrictNameDirMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "dir-name")
	os.MkdirAll(skillDir, 0755)

	skillContent := "---\nname: different-name\ndescription: Name doesn't match dir\n---\n\n# Different"
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)

	src := NewLocalDirSource("test-mismatch", DiscoveryKindSkill, tmpDir, true)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items (name/dir mismatch), got %d", len(items))
	}
}

func TestLocalDirSource_Discover_NonStrictAcceptsInvalid(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "BadName")
	os.MkdirAll(skillDir, 0755)

	skillContent := "---\nname: BadName\ndescription: A skill with invalid name\n---\n\n# BadName"
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)

	src := NewLocalDirSource("test-nonstrict", DiscoveryKindSkill, tmpDir, false)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (non-strict accepts), got %d", len(items))
	}
}

func TestLocalDirSource_Discover_MultipleSkills(t *testing.T) {
	tmpDir := t.TempDir()

	for _, name := range []string{"code-review", "web-scraper", "db-query"} {
		skillDir := filepath.Join(tmpDir, name)
		os.MkdirAll(skillDir, 0755)
		skillContent := "---\nname: " + name + "\ndescription: Skill " + name + "\n---\n\n# " + name
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)
	}

	src := NewLocalDirSource("test-multi", DiscoveryKindSkill, tmpDir, false)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
}

func TestLocalDirSource_Discover_SkipsNonSKILLMD(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "code-review")
	os.MkdirAll(skillDir, 0755)

	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: code-review\ndescription: Review code\n---\n\n# Code Review"), 0644)
	os.WriteFile(filepath.Join(skillDir, "README.md"), []byte("# README"), 0644)
	os.WriteFile(filepath.Join(skillDir, "script.sh"), []byte("echo hello"), 0644)

	src := NewLocalDirSource("test-skip", DiscoveryKindSkill, tmpDir, false)
	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (only SKILL.md), got %d", len(items))
	}
}

func TestLocalDirSource_Name(t *testing.T) {
	src := NewLocalDirSource("my-local", DiscoveryKindSkill, "/tmp", false)
	if src.Name() != "my-local" {
		t.Errorf("expected Name() 'my-local', got %q", src.Name())
	}
}

func TestLocalDirSource_Kind(t *testing.T) {
	src := NewLocalDirSource("my-local", DiscoveryKindSkill, "/tmp", false)
	if src.Kind() != DiscoveryKindSkill {
		t.Errorf("expected Kind() %q, got %q", DiscoveryKindSkill, src.Kind())
	}
}

func TestDiscoveredItemToSkillWithDir(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "test-skill")
	os.MkdirAll(skillDir, 0755)

	item := DiscoveredItem{
		Kind:        DiscoveryKindSkill,
		ID:          "ext_test_skill",
		Name:        "test-skill",
		Capability:  "testing",
		Description: "A test skill",
		Implementations: map[string]config.SkillImplementation{
			"default": {SystemPrompt: "You are a tester"},
		},
	}

	skill := discoveredItemToSkillWithDir(item, skillDir)
	if skill.ID != "ext_test_skill" {
		t.Errorf("expected ID 'ext_test_skill', got %q", skill.ID)
	}
	if skill.ExecutionMode != config.ExecutionModeDirectory {
		t.Errorf("expected ExecutionMode %q, got %q", config.ExecutionModeDirectory, skill.ExecutionMode)
	}
	if skill.SkillDir != skillDir {
		t.Errorf("expected SkillDir %q, got %q", skillDir, skill.SkillDir)
	}
	if skill.Domain != "EXTERNAL" {
		t.Errorf("expected Domain 'EXTERNAL', got %q", skill.Domain)
	}
	if skill.Source != config.SourceExternalDynamic {
		t.Errorf("expected Source %q, got %q", config.SourceExternalDynamic, skill.Source)
	}
}
