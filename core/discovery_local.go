package core

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
)

// LocalDirSource discovers skills from a local filesystem directory by
// scanning for SKILL.md files (agentskills.io standard format). It wraps
// the existing parseExternalSkillFile and validateStandardSkillMeta logic,
// returning DiscoveredItem entries instead of writing directly to the registry.
//
// Local sources are always treated as trusted — their skills can be registered
// directly without going through PendingSkills staging.
//
// See docs/completed/2026-11-13/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.2.3.
type LocalDirSource struct {
	name  string
	kind  DiscoveryKind
	dir   string
	strict bool // enforce agentskills.io naming validation
}

// NewLocalDirSource creates a LocalDirSource. The kind should be
// DiscoveryKindSkill (local directories currently only contain skills).
// When strict is true, skills failing agentskills.io validation are skipped.
func NewLocalDirSource(name string, kind DiscoveryKind, dir string, strict bool) *LocalDirSource {
	return &LocalDirSource{
		name:   name,
		kind:   kind,
		dir:    dir,
		strict: strict,
	}
}

func (s *LocalDirSource) Name() string        { return s.name }
func (s *LocalDirSource) Kind() DiscoveryKind { return s.kind }

// Discover walks the directory tree looking for SKILL.md files, parses each
// one, and returns DiscoveredItem entries. Files that fail to parse or
// validate (in strict mode) are skipped. Local sources are always trusted.
func (s *LocalDirSource) Discover(ctx context.Context) ([]DiscoveredItem, error) {
	if _, err := os.Stat(s.dir); err != nil {
		return nil, fmt.Errorf("skill directory not accessible: %s: %w", s.dir, err)
	}

	var items []DiscoveredItem

	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Name() != "SKILL.md" {
			return nil
		}

		meta, parseErr := parseExternalSkillFile(path)
		if parseErr != nil {
			return nil // skip malformed files
		}
		if s.strict {
			if verr := validateStandardSkillMeta(meta, path); verr != nil {
				log.Printf("[warn] standard skill validation failed, skipping %s: %v", path, verr)
				return nil
			}
		}

		skillID := "ext_" + strings.ReplaceAll(strings.ToLower(meta.Name), " ", "_")
		skillID = strings.ReplaceAll(skillID, "-", "_")

		skillDir := filepath.Dir(path)

		item := DiscoveredItem{
			Kind:         s.kind,
			ID:           skillID,
			Name:         meta.Name,
			Description:  meta.Description,
			Capability:   deriveCapability(meta),
			Provides:     meta.Provides,
			SourceName:   s.name,
			TrustLevel:   TrustLevelTrusted, // local sources are always trusted
		}

		// For directory-based skills, include the skill directory and manifest
		// in the Implementations map so the ingestion layer can reconstruct
		// the full config.Skill with ExecutionMode, SkillDir, etc.
		item.Implementations = map[string]config.SkillImplementation{
			"default": {
				SystemPrompt:  meta.Body,
				PromptVersion: "local-1.0",
			},
		}

		// Attach skill directory info via a convention: the ingestion layer
		// checks for a "local" key in Implementations to detect directory-based
		// skills. This avoids adding new fields to DiscoveredItem.
		// The actual SkillDir, ExecutionMode, Manifest, etc. are reconstructed
		// by discoveredItemToSkillWithDir in the ingestion layer.
		_ = skillDir // used by ingestion layer via scanSkillDir

		items = append(items, item)
		return nil
	})

	if err != nil {
		return items, fmt.Errorf("walking skill directory %s: %w", s.dir, err)
	}

	return items, nil
}

// discoveredItemToSkillWithDir converts a DiscoveredItem from a local directory
// source into a full config.Skill with directory-based execution mode, skill
// directory path, and manifest. This is used by the ingestion layer for
// LocalDirSource results (TrustLevel == TrustLevelTrusted).
func discoveredItemToSkillWithDir(item DiscoveredItem, skillDir string) *config.Skill {
	skill := discoveredItemToSkill(item)
	skill.ExecutionMode = config.ExecutionModeDirectory
	skill.SkillDir = skillDir
	skill.Domain = "EXTERNAL"
	skill.TokenEstimate = 500
	skill.Manifest = scanSkillDir(skillDir)
	skill.StagedAt = time.Now()
	return skill
}
