package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_SkillSyncSources_ConvertedToDiscoverySources verifies that legacy
// skill_sync_sources entries in config.json are automatically converted to
// discovery_sources entries during config loading.
//
// See docs/completed/2026-11-13/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.3.2.
func TestLoad_SkillSyncSources_ConvertedToDiscoverySources(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
	  "default_provider": "p1",
	  "providers": {"p1": {"provider": "openai", "model": "m", "api_key": "k"}},
	  "skill_sync_sources": {
	    "general": "github:mattpocock/skills",
	    "google": "github:google/skills@ai"
	  }
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	// Legacy sources should be converted to DiscoverySources with name "legacy-<category>".
	if len(reg.System.DiscoverySources) != 2 {
		t.Fatalf("expected 2 DiscoverySources (converted from SkillSyncSources), got %d",
			len(reg.System.DiscoverySources))
	}

	// Find the converted entries by name.
	foundGeneral := false
	foundGoogle := false
	for _, ds := range reg.System.DiscoverySources {
		if ds.Name == "legacy-general" {
			foundGeneral = true
			if ds.Type != "github" {
				t.Errorf("legacy-general: expected type github, got %q", ds.Type)
			}
			if ds.Kind != "skill" {
				t.Errorf("legacy-general: expected kind skill, got %q", ds.Kind)
			}
			if ds.URL != "github:mattpocock/skills" {
				t.Errorf("legacy-general: expected URL github:mattpocock/skills, got %q", ds.URL)
			}
		}
		if ds.Name == "legacy-google" {
			foundGoogle = true
			if ds.Type != "github" {
				t.Errorf("legacy-google: expected type github, got %q", ds.Type)
			}
			if ds.Kind != "skill" {
				t.Errorf("legacy-google: expected kind skill, got %q", ds.Kind)
			}
			if ds.URL != "github:google/skills@ai" {
				t.Errorf("legacy-google: expected URL github:google/skills@ai, got %q", ds.URL)
			}
		}
	}
	if !foundGeneral {
		t.Error("legacy-general not found in DiscoverySources")
	}
	if !foundGoogle {
		t.Error("legacy-google not found in DiscoverySources")
	}

	// SkillSyncSources should also be preserved on the Registry for backward compat.
	if reg.SkillSyncSources == nil {
		t.Fatal("SkillSyncSources should be preserved on Registry for backward compat")
	}
	if len(reg.SkillSyncSources) != 2 {
		t.Fatalf("expected 2 SkillSyncSources entries preserved, got %d", len(reg.SkillSyncSources))
	}
}

// TestLoad_SkillSyncSources_NoDuplicateWithDiscoverySources verifies that
// when both skill_sync_sources and discovery_sources are present in config,
// the legacy entries are not duplicated if a matching discovery_source
// already exists (matched by name).
func TestLoad_SkillSyncSources_NoDuplicateWithDiscoverySources(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
	  "default_provider": "p1",
	  "providers": {"p1": {"provider": "openai", "model": "m", "api_key": "k"}},
	  "skill_sync_sources": {
	    "general": "github:mattpocock/skills"
	  },
	  "system": {
	    "discovery_sources": [
	      {"name": "legacy-general", "type": "github", "kind": "skill", "url": "github:mattpocock/skills"}
	    ]
	  }
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	// Should have exactly 1 DiscoverySource (the explicit one), not 2.
	if len(reg.System.DiscoverySources) != 1 {
		t.Fatalf("expected 1 DiscoverySource (no duplicate), got %d",
			len(reg.System.DiscoverySources))
	}
	if reg.System.DiscoverySources[0].Name != "legacy-general" {
		t.Errorf("expected legacy-general, got %q", reg.System.DiscoverySources[0].Name)
	}
}

// TestLoad_NoSkillSyncSources_NoExtraDiscoverySources verifies that when
// skill_sync_sources is absent, no extra DiscoverySources are added.
func TestLoad_NoSkillSyncSources_NoExtraDiscoverySources(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
	  "default_provider": "p1",
	  "providers": {"p1": {"provider": "openai", "model": "m", "api_key": "k"}},
	  "system": {
	    "discovery_sources": [
	      {"name": "http-mcp", "type": "http", "kind": "mcp", "url": "https://example.com/api"}
	    ]
	  }
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	reg, err := NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	if len(reg.System.DiscoverySources) != 1 {
		t.Fatalf("expected 1 DiscoverySource, got %d", len(reg.System.DiscoverySources))
	}
	if reg.System.DiscoverySources[0].Name != "http-mcp" {
		t.Errorf("expected http-mcp, got %q", reg.System.DiscoverySources[0].Name)
	}
}
