package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// SkillHashManifest is a mapping of skillID → content hash, persisted to
// workspace/skills/.manifest.json. It detects cross-session skill version
// drift: if a skill's Implementations change between loads, the hash changes
// and a warning is logged.
//
// Design: docs/plan/RESIDUAL_ISSUES_FIX.md §3.
type SkillHashManifest map[string]string

// manifestFileName is the manifest file written to the skills directory.
const manifestFileName = ".manifest.json"

// ComputeSkillHash returns the SHA-256 hash of a skill's Implementations
// field (JSON-marshaled). This captures the executable content — metadata
// changes (Name, Description) don't affect the hash.
func ComputeSkillHash(s *Skill) string {
	if s == nil {
		return ""
	}
	data, err := json.Marshal(s.Implementations)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// LoadManifest reads the manifest from the given skills directory.
// Returns an empty manifest if the file doesn't exist or is unreadable.
func LoadManifest(skillsDir string) SkillHashManifest {
	path := filepath.Join(skillsDir, manifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillHashManifest{}
	}
	var m SkillHashManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return SkillHashManifest{}
	}
	return m
}

// WriteManifest writes the manifest to the given skills directory.
// Best-effort: errors are logged but not returned (manifest is advisory).
func WriteManifest(skillsDir string, m SkillHashManifest) {
	path := filepath.Join(skillsDir, manifestFileName)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[warn] could not write skill manifest to %s: %v", path, err)
	}
}

// CheckSkillDrift compares the currently loaded skills against the manifest
// on disk. Logs warnings for:
//   - Skills whose content hash changed since last load (version drift)
//   - Skills in the manifest but no longer in the registry (removed)
//
// After checking, writes the current manifest so the next load can detect
// drift against this baseline.
//
// This is advisory only — it logs warnings but does not change behavior.
//
// Caller must hold reg.Mu (RLock or Lock) — this function does NOT acquire
// the mutex itself, to avoid deadlock when called from loadLocked.
func CheckSkillDrift(reg *Registry, skillsDir string) {
	if reg == nil {
		return
	}

	old := LoadManifest(skillsDir)
	current := SkillHashManifest{}

	for id, s := range reg.Skills {
		if s == nil {
			continue
		}
		hash := ComputeSkillHash(s)
		current[id] = hash

		if prevHash, ok := old[id]; ok {
			if prevHash != hash {
				log.Printf("[warn] skill %q version drift: content changed since last load", id)
			}
		}
	}

	// Check for removed skills (in manifest but not in registry).
	for id := range old {
		if _, ok := current[id]; !ok {
			log.Printf("[warn] skill %q was in manifest but not loaded — may have been removed", id)
		}
	}

	WriteManifest(skillsDir, current)
}
