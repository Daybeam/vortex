package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestComputeSkillHash_Deterministic(t *testing.T) {
	s := &Skill{
		ID: "test_skill",
		Implementations: map[string]SkillImplementation{
			"python": {SystemPrompt: "print('hello')"},
		},
	}
	h1 := ComputeSkillHash(s)
	h2 := ComputeSkillHash(s)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s != %s", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty for non-nil skill")
	}
}

func TestComputeSkillHash_ChangesOnImplementationChange(t *testing.T) {
	s1 := &Skill{
		ID: "test_skill",
		Implementations: map[string]SkillImplementation{
			"python": {SystemPrompt: "print('hello')"},
		},
	}
	s2 := &Skill{
		ID: "test_skill",
		Implementations: map[string]SkillImplementation{
			"python": {SystemPrompt: "print('world')"},
		},
	}
	if ComputeSkillHash(s1) == ComputeSkillHash(s2) {
		t.Error("hash should differ when Implementations differ")
	}
}

func TestComputeSkillHash_NilSafe(t *testing.T) {
	if ComputeSkillHash(nil) != "" {
		t.Error("nil skill should produce empty hash")
	}
}

func TestComputeSkillHash_MatchesManualSHA256(t *testing.T) {
	s := &Skill{
		ID: "test_skill",
		Implementations: map[string]SkillImplementation{
			"python": {SystemPrompt: "x = 1"},
		},
	}
	data, _ := json.Marshal(s.Implementations)
	expected := sha256.Sum256(data)
	expectedHex := hex.EncodeToString(expected[:])
	if ComputeSkillHash(s) != expectedHex {
		t.Error("hash should match manual SHA-256 of Implementations JSON")
	}
}

func TestManifestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := SkillHashManifest{
		"skill_a": "hash_a",
		"skill_b": "hash_b",
	}
	WriteManifest(dir, original)
	loaded := LoadManifest(dir)
	if loaded["skill_a"] != "hash_a" || loaded["skill_b"] != "hash_b" {
		t.Errorf("round-trip failed: got %v", loaded)
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	dir := t.TempDir()
	m := LoadManifest(dir)
	if len(m) != 0 {
		t.Errorf("missing manifest should return empty map, got %v", m)
	}
}

func TestCheckSkillDrift_WritesManifest(t *testing.T) {
	dir := t.TempDir()
	reg := &Registry{
		Skills: map[string]*Skill{
			"skill_a": {
				ID: "skill_a",
				Implementations: map[string]SkillImplementation{
					"python": {SystemPrompt: "x = 1"},
				},
			},
		},
	}
	// CheckSkillDrift assumes caller holds the lock (called from loadLocked).
	reg.Mu.RLock()
	CheckSkillDrift(reg, dir)
	reg.Mu.RUnlock()

	// Manifest should now exist with skill_a's hash
	manifestPath := filepath.Join(dir, manifestFileName)
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		t.Fatal("CheckSkillDrift should have written a manifest file")
	}
	m := LoadManifest(dir)
	if _, ok := m["skill_a"]; !ok {
		t.Error("manifest should contain skill_a")
	}
}

func TestCheckSkillDrift_NilSafe(t *testing.T) {
	dir := t.TempDir()
	CheckSkillDrift(nil, dir) // should not panic
}
