package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/config"
)

// GitHubRepoSource discovers skills from a GitHub repository by downloading
// the repo tarball (preserving full directory structure including scripts/,
// assets/, references/) and walking for SKILL.md files. This fixes Gap 3.1
// from SKILL_MARKETPLACE_REPO_CONTRACT_DESIGN.md — the old approach only
// downloaded .md/.json files via individual API calls, silently dropping
// directory-based skill content.
//
// See docs/completed/2026-11-13/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.2.1.
type GitHubRepoSource struct {
	name   string
	kind   DiscoveryKind
	owner  string
	repo   string
	path   string
	loader *ResourceLoader
}

// NewGitHubRepoSource creates a GitHubRepoSource. The kind should be
// DiscoveryKindSkill (GitHub repos currently only contain skills).
// The source URL is parsed by parseGitHubRepoSource to extract owner/repo/path.
func NewGitHubRepoSource(name string, kind DiscoveryKind, sourceURL string, loader *ResourceLoader) (*GitHubRepoSource, error) {
	owner, repo, path, isRepo := parseGitHubRepoSource(sourceURL)
	if !isRepo {
		return nil, fmt.Errorf("invalid GitHub repo source format: %q (expected github:owner/repo or https://github.com/owner/repo)", sourceURL)
	}
	return &GitHubRepoSource{
		name:   name,
		kind:   kind,
		owner:  owner,
		repo:   repo,
		path:   path,
		loader: loader,
	}, nil
}

func (s *GitHubRepoSource) Name() string        { return s.name }
func (s *GitHubRepoSource) Kind() DiscoveryKind { return s.kind }

// Discover downloads the GitHub repo tarball, extracts it to a persistent
// cache directory, and walks for SKILL.md files (directory-based skills) and
// top-level .md/.json files (prompt-only skills). SKILL.md files carry
// SkillDir so the ingestion layer can set ExecutionMode/Manifest; prompt-only
// files have empty SkillDir (backward compat with the old flat-file approach).
//
// Gap 3.1 fix: tarball preserves scripts/assets/references that the old
// per-file API approach silently dropped.
func (s *GitHubRepoSource) Discover(ctx context.Context) ([]DiscoveredItem, error) {
	cacheDir := filepath.Join(os.TempDir(), "orch-skills-cache", s.owner+"-"+s.repo)
	if err := s.loader.FetchArchive(ctx, s.owner, s.repo, cacheDir); err != nil {
		return nil, fmt.Errorf("fetching archive for %s/%s: %w", s.owner, s.repo, err)
	}

	walkDir := cacheDir
	if s.path != "" {
		walkDir = filepath.Join(cacheDir, s.path)
	}
	if _, err := os.Stat(walkDir); os.IsNotExist(err) {
		return nil, nil
	}

	var items []DiscoveredItem
	err := filepath.Walk(walkDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		if info.Name() == "SKILL.md" {
			meta, parseErr := parseExternalSkillFile(path)
			if parseErr != nil {
				return nil
			}
			skillDir := filepath.Dir(path)
			skillID := "ext_" + strings.ReplaceAll(strings.ToLower(meta.Name), " ", "_")
			skillID = strings.ReplaceAll(skillID, "-", "_")
			item := DiscoveredItem{
				Kind:        s.kind,
				ID:          skillID,
				Name:        meta.Name,
				Description: meta.Description,
				Capability:  deriveCapability(meta),
				Implementations: map[string]config.SkillImplementation{
					"default": {
						SystemPrompt:  meta.Body,
						PromptVersion: "remote-1.0",
					},
				},
				SkillDir:   skillDir,
				SourceName: s.name,
				TrustLevel: TrustLevelUntrusted,
			}
			items = append(items, item)
			return nil
		}

		if filepath.Dir(path) == walkDir {
			if strings.HasSuffix(info.Name(), ".md") || strings.HasSuffix(info.Name(), ".json") {
				content, readErr := os.ReadFile(path)
				if readErr != nil {
					return nil
				}
				item, parseErr := parseSkillFileToDiscoveredItem(info.Name(), string(content))
				if parseErr != nil {
					return nil
				}
				item.SourceName = s.name
				item.Kind = s.kind
				if item.TrustLevel == "" {
					item.TrustLevel = TrustLevelUntrusted
				}
				items = append(items, *item)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking skill directory: %w", err)
	}

	return items, nil
}

// parseSkillFileToDiscoveredItem parses a .md or .json file into a
// DiscoveredItem. Used for top-level prompt-only skill files (not SKILL.md).
//
// For .json files: unmarshals directly into DiscoveredItem (which carries
// the same skill fields as config.Skill).
// For .md files: uses the same heuristic as parseSkill — extract ID from
// filename, name from first "# " heading, wrap content as default implementation.
func parseSkillFileToDiscoveredItem(filename, content string) (*DiscoveredItem, error) {
	if strings.HasSuffix(filename, ".json") {
		var item DiscoveredItem
		if err := json.Unmarshal([]byte(content), &item); err == nil && item.ID != "" {
			return &item, nil
		}
		var skill config.Skill
		if err := json.Unmarshal([]byte(content), &skill); err == nil {
			return skillToDiscoveredItem(&skill), nil
		}
	}

	id := strings.TrimSuffix(filename, filepath.Ext(filename))
	id = strings.ReplaceAll(id, " ", "_")
	id = strings.ToLower(id)

	name := id
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			name = strings.TrimPrefix(line, "# ")
			break
		}
	}

	return &DiscoveredItem{
		Kind:     DiscoveryKindSkill,
		ID:       id,
		Name:     name,
		Capability: "general",
		Description: "Imported from remote repository",
		Implementations: map[string]config.SkillImplementation{
			"default": {
				SystemPrompt:  content,
				PromptVersion: "remote-1.0",
			},
		},
	}, nil
}

// skillToDiscoveredItem converts a config.Skill to a DiscoveredItem.
// Used when parsing JSON skill files that were originally written as
// config.Skill structs (the existing data/skills/*.json format).
func skillToDiscoveredItem(skill *config.Skill) *DiscoveredItem {
	return &DiscoveredItem{
		Kind:           DiscoveryKindSkill,
		ID:             skill.ID,
		Name:           skill.Name,
		Description:    skill.Description,
		Capability:     skill.Capability,
		Implementations: skill.Implementations,
	}
}

// discoveredItemToSkill converts a DiscoveredItem to a config.Skill.
// When SkillDir is non-empty (directory-based skill from tarball download),
// sets ExecutionMode=Directory, SkillDir, and scans for Manifest content
// (scripts/assets/references). This fixes Gap 3.1 — directory-based skills
// now carry their full content instead of being silently stripped to prompt-only.
func discoveredItemToSkill(item DiscoveredItem) *config.Skill {
	skill := &config.Skill{
		ID:             item.ID,
		Name:           item.Name,
		Capability:     item.Capability,
		Description:    item.Description,
		Source:         config.SourceExternalDynamic,
		Implementations: item.Implementations,
	}
	if item.SkillDir != "" {
		skill.ExecutionMode = config.ExecutionModeDirectory
		skill.SkillDir = item.SkillDir
		skill.Manifest = scanSkillDir(item.SkillDir)
	}
	return skill
}
