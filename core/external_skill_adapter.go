package core

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ExternalSkillAdapter imports external agentskills.io compliant SKILL.md bundles
// into Orchestrator's native config.Skill and SOP registries.
type ExternalSkillAdapter struct {
	Registry *config.Registry
}

func NewExternalSkillAdapter(reg *config.Registry) *ExternalSkillAdapter {
	return &ExternalSkillAdapter{Registry: reg}
}

type ExternalSkillMeta struct {
	Name        string
	Description string
	Body        string
	Provides    []string
	Capability  string // explicit frontmatter `capability:` field

	// agentskills.io standard fields (docs/plan/AGENTIC_SKILL_EXPLORATION_DESIGN.md)
	Compatibility string            // environment requirements; surfaced as a warning at activation
	AllowedTools  []string          // space-separated pre-approved tool list; advisory only
	License       string            // license name; audit/drift only
	Metadata      map[string]string // free-form string map; audit/drift only
}

// ImportSkillDirectory scans a directory of skills (e.g. mattpocock/skills/skills/)
// and registers them into the Orchestrator registry. Non-compliant skill names
// (spaces, capitals) are tolerated — legacy marketplace repos don't follow the
// agentskills.io naming rules.
func (a *ExternalSkillAdapter) ImportSkillDirectory(rootPath string) (int, error) {
	return a.walkImport(rootPath, false)
}

// ImportStandardSkills is the strict variant used for agentskills.io standard
// discovery paths (`.agents/skills/`): skills failing validation (name/dir
// mismatch, name charset or length, description length) are skipped with a
// warning instead of imported.
func (a *ExternalSkillAdapter) ImportStandardSkills(rootPath string) (int, error) {
	return a.walkImport(rootPath, true)
}

func (a *ExternalSkillAdapter) walkImport(rootPath string, strict bool) (int, error) {
	if _, err := os.Stat(rootPath); os.IsNotExist(err) {
		return 0, fmt.Errorf("skill directory not found: %s", rootPath)
	}

	importedCount := 0

	err := filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Name() != "SKILL.md" {
			return nil
		}

		meta, parseErr := parseExternalSkillFile(path)
		if parseErr != nil {
			return nil // skip malformed
		}
		if strict {
			if verr := validateStandardSkillMeta(meta, path); verr != nil {
				log.Printf("[warn] standard skill validation failed, skipping %s: %v", path, verr)
				return nil
			}
		}

		skillID := "ext_" + strings.ReplaceAll(strings.ToLower(meta.Name), " ", "_")
		skillID = strings.ReplaceAll(skillID, "-", "_")

		// 1. Register as native Skill
		skillDir := filepath.Dir(path)
		skill := &config.Skill{
			ID:            skillID,
			Name:          meta.Name,
			Description:   meta.Description,
			Capability:    deriveCapability(meta),
			Domain:        "EXTERNAL",
			TokenEstimate: 500,
			ExecutionMode: config.ExecutionModeDirectory,
			SkillDir:      skillDir,
			Provides:      meta.Provides,
			Manifest:      scanSkillDir(skillDir),
			StagedAt:      time.Now(),

			Compatibility: meta.Compatibility,
			AllowedTools:  meta.AllowedTools,
			License:       meta.License,
			Metadata:      meta.Metadata,
		}

		a.Registry.Mu.Lock()
		// TODO-4: External skills go to PendingSkills (staging), not Skills.
		// This makes them invisible to CapabilityIndex.Rebuild and all retrievers
		// (which only read Skills), so they are not discoverable or injectable
		// until explicitly promoted via the promote_skill action.
		// See docs/plan/SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-4.
		if a.Registry.PendingSkills == nil {
			a.Registry.PendingSkills = make(map[string]*config.Skill)
		}
		a.Registry.PendingSkills[skillID] = skill

		// 2. If it's an orchestrator-style multi-step skill, also register as SOP
		if strings.Contains(meta.Body, "## ") || strings.Contains(meta.Body, "1.") {
			if a.Registry.SOPs == nil {
				a.Registry.SOPs = make(map[string]*schemas.SOP)
			}
			a.Registry.SOPs[skillID] = &schemas.SOP{
				ID:          skillID,
				Description: meta.Description,
				Triggers:    []schemas.SOPTrigger{{Keywords: []string{meta.Name}}},
				Steps: map[string]schemas.SOPStep{
					"execute_" + skillID: {
						ID:   "execute_" + skillID,
						Role: "subagent",
						Task: meta.Body,
					},
				},
			}
		}
		a.Registry.Mu.Unlock()

		importedCount++
		return nil
	})

	if err != nil {
		return importedCount, err
	}

	return importedCount, nil
}

func parseExternalSkillFile(path string) (*ExternalSkillMeta, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// FIX (2026-10-09): Default bufio.MaxScanTokenSize is 64KB. A 57KB+ SKILL.md
	// with a long single line (e.g. minified code block) would silently fail.
	// Enlarge to 1MB to handle large skills without modifying the original file.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inFrontmatter := false
	var frontmatterLines []string
	var bodyLines []string

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			} else {
				inFrontmatter = false
				continue
			}
		}

		if inFrontmatter {
			frontmatterLines = append(frontmatterLines, line)
		} else {
			bodyLines = append(bodyLines, line)
		}
	}

	// audit L-NEW-10: check scanner.Err() — if a line exceeds the 1MB buffer,
	// the scanner stops silently and returns truncated content.
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse skill file %q: %w", path, err)
	}

	name := filepath.Base(filepath.Dir(path))
	description := ""
	capability := ""
	compatibility := ""
	license := ""
	var provides []string
	var allowedTools []string
	var metadata map[string]string
	inMetadata := false

	for _, fl := range frontmatterLines {
		// Nested `metadata:` map entries — indented `key: value` lines under a
		// top-level `metadata:` key (agentskills.io standard form).
		if inMetadata {
			if strings.HasPrefix(fl, " ") || strings.HasPrefix(fl, "\t") {
				kv := strings.SplitN(fl, ":", 2)
				if len(kv) == 2 {
					mk := strings.TrimSpace(kv[0])
					mv := strings.Trim(strings.TrimSpace(kv[1]), "\"'")
					if mk != "" {
						if metadata == nil {
							metadata = make(map[string]string)
						}
						metadata[mk] = mv
					}
				}
				continue
			}
			inMetadata = false
		}

		parts := strings.SplitN(fl, ":", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, "\"'")
			if k == "name" {
				name = v
			} else if k == "description" {
				description = v
			} else if k == "provides" {
				provides = parseProvidesList(v)
			} else if k == "capability" {
				capability = v
			} else if k == "compatibility" {
				compatibility = v
			} else if k == "allowed-tools" {
				allowedTools = strings.Fields(v)
			} else if k == "license" {
				license = v
			} else if k == "metadata" {
				inMetadata = true
			}
		}
	}

	return &ExternalSkillMeta{
		Name:          name,
		Description:   description,
		Body:          strings.Join(bodyLines, "\n"),
		Provides:      provides,
		Capability:    capability,
		Compatibility: compatibility,
		AllowedTools:  allowedTools,
		License:       license,
		Metadata:      metadata,
	}, nil
}

// validateStandardSkillMeta enforces the agentskills.io constraints for skills
// imported from standard discovery paths (docs/plan/AGENTIC_SKILL_EXPLORATION_DESIGN.md G5):
// name must match the parent directory, be ≤64 chars of [a-z0-9-] with sane
// hyphen placement, and description must be non-empty and ≤1024 chars.
func validateStandardSkillMeta(meta *ExternalSkillMeta, skillPath string) error {
	dirName := filepath.Base(filepath.Dir(skillPath))
	if meta.Name != dirName {
		return fmt.Errorf("name %q does not match parent directory %q", meta.Name, dirName)
	}
	if len(meta.Name) > 64 {
		return fmt.Errorf("name exceeds 64 characters (%d)", len(meta.Name))
	}
	if !validStandardSkillName(meta.Name) {
		return fmt.Errorf("name %q violates [a-z0-9-] constraints", meta.Name)
	}
	if meta.Description == "" {
		return fmt.Errorf("description is required")
	}
	if len(meta.Description) > 1024 {
		return fmt.Errorf("description exceeds 1024 characters (%d)", len(meta.Description))
	}
	return nil
}

// validStandardSkillName reports whether s is lowercase alphanumeric with
// single, non-edge hyphens (agentskills.io: `^[a-z0-9]+(-[a-z0-9]+)*$`).
func validStandardSkillName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

// ImportStandardDiscoveryPaths imports skills from the agentskills.io standard
// discovery paths: project-level <projectDir>/.agents/skills/ and user-level
// <userDir>/.agents/skills/ (empty string skips that level). Skills land in
// PendingSkills staging — they become discoverable only after promote_skill,
// preserving the unverified-external-skill gate. Returns total imported count.
func ImportStandardDiscoveryPaths(reg *config.Registry, projectDir, userDir string) int {
	if !config.AllowExternalAssets() {
		return 0
	}
	adapter := NewExternalSkillAdapter(reg)
	n := 0
	for _, dir := range []string{projectDir, userDir} {
		if dir == "" {
			continue
		}
		skillsDir := filepath.Join(dir, ".agents", "skills")
		if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
			continue
		}
		count, err := adapter.ImportStandardSkills(skillsDir)
		if err != nil {
			log.Printf("[warn] standard skill import failed for %s: %v", skillsDir, err)
			continue
		}
		n += count
	}
	return n
}

// deriveCapability determines the capability tag for an external skill.
// Priority: explicit frontmatter `capability:` → first meaningful word from
// description → generic "external" fallback.
//
// This replaces the previous hardcoded "external_discipline" which caused all
// external skills to share a single tag, making SemanticRetriever return every
// external skill on any query matching "external" or "discipline" (noise,
// not signal). See docs/plan/SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-1.
func deriveCapability(meta *ExternalSkillMeta) string {
	// 1. Explicit frontmatter capability
	if meta.Capability != "" {
		return strings.ToLower(meta.Capability)
	}

	// 2. First meaningful word from description (≥3 bytes, lowercased)
	if meta.Description != "" {
		for _, w := range strings.Fields(meta.Description) {
			w = strings.ToLower(strings.Trim(w, ".,;:!?\"'()[]"))
			if len(w) >= 3 {
				return w
			}
		}
	}

	// 3. Generic fallback — "external" (not "external_discipline")
	return "external"
}

// parseProvidesList parses a YAML-style list value like "[a, b, c]" or "a, b, c".
func parseProvidesList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"'")
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// scanSkillDir builds a SkillManifest by scanning references/, scripts/, assets/
// subdirectories. Only one level deep (no recursion). Capped at 50 total items.
func scanSkillDir(rootDir string) *config.SkillManifest {
	m := &config.SkillManifest{}
	total := 0
	const maxItems = 50

	for _, sub := range []struct{ dir, field string }{
		{"references", "references"},
		{"scripts", "scripts"},
		{"assets", "assets"},
	} {
		if total >= maxItems {
			break
		}
		entries, err := os.ReadDir(filepath.Join(rootDir, sub.dir))
		if err != nil {
			continue // dir doesn't exist or unreadable — skip
		}
		for _, e := range entries {
			if e.IsDir() {
				continue // one level deep only
			}
			if total >= maxItems {
				break
			}
			relPath := filepath.Join(sub.dir, e.Name())
			switch sub.field {
			case "references":
				m.References = append(m.References, relPath)
			case "scripts":
				m.Scripts = append(m.Scripts, relPath)
			case "assets":
				m.Assets = append(m.Assets, relPath)
			}
			total++
		}
	}

	// Return nil if manifest is empty (keeps Skill struct clean)
	if total == 0 {
		return nil
	}
	return m
}
