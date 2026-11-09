package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/config"
)

// GitHubRepoSource discovers skills from a GitHub repository by listing its
// contents via the GitHub API and fetching .md/.json files. It wraps the
// existing ResourceLoader.FetchRepoContents and parseSkillFile logic,
// returning DiscoveredItem entries instead of writing directly to the registry.
//
// See docs/plan/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.2.1.
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

// Discover fetches the GitHub repo contents, downloads each .md/.json file,
// and parses them into DiscoveredItem entries. Files that fail to fetch or
// parse are skipped (logged by the caller), not fatal.
func (s *GitHubRepoSource) Discover(ctx context.Context) ([]DiscoveredItem, error) {
	files, err := s.loader.FetchRepoContents(ctx, s.owner, s.repo, s.path)
	if err != nil {
		return nil, fmt.Errorf("fetching repo contents for %s/%s: %w", s.owner, s.repo, err)
	}

	var items []DiscoveredItem
	for _, file := range files {
		if file.Type != "file" {
			continue
		}
		if !strings.HasSuffix(file.Name, ".md") && !strings.HasSuffix(file.Name, ".json") {
			continue
		}

		content, err := s.loader.Fetch(ctx, file.DownloadURL)
		if err != nil {
			continue // skip unreadable files
		}

		item, err := parseSkillFileToDiscoveredItem(file.Name, content)
		if err != nil {
			continue // skip unparseable files
		}

		item.SourceName = s.name
		item.Kind = s.kind
		if item.TrustLevel == "" {
			item.TrustLevel = TrustLevelUntrusted
		}
		items = append(items, *item)
	}

	return items, nil
}

// parseSkillFileToDiscoveredItem parses a .md or .json file into a
// DiscoveredItem. This is the shared parsing logic extracted from
// SkillSyncer.parseSkill, producing DiscoveredItem instead of config.Skill
// so it can be used by both GitHubRepoSource and the legacy SkillSyncer.
//
// For .json files: unmarshals directly into DiscoveredItem (which carries
// the same skill fields as config.Skill).
// For .md files: uses the same heuristic as parseSkill — extract ID from
// filename, name from first "# " heading, wrap content as default implementation.
func parseSkillFileToDiscoveredItem(filename, content string) (*DiscoveredItem, error) {
	// JSON files: try to unmarshal as a DiscoveredItem first, then fall back
	// to config.Skill (for backward compat with existing skill JSON files).
	if strings.HasSuffix(filename, ".json") {
		var item DiscoveredItem
		if err := json.Unmarshal([]byte(content), &item); err == nil && item.ID != "" {
			return &item, nil
		}
		// Fall back: try as config.Skill, then convert
		var skill config.Skill
		if err := json.Unmarshal([]byte(content), &skill); err == nil {
			return skillToDiscoveredItem(&skill), nil
		}
	}

	// Markdown files: heuristic parse (same as SkillSyncer.parseSkill)
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
// Used by the ingestion layer when registering discovered skills.
func discoveredItemToSkill(item DiscoveredItem) *config.Skill {
	return &config.Skill{
		ID:             item.ID,
		Name:           item.Name,
		Capability:     item.Capability,
		Description:    item.Description,
		Source:         config.SourceExternalDynamic,
		Implementations: item.Implementations,
	}
}
