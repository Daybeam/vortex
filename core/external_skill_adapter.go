package core

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
}

// ImportSkillDirectory scans a directory of skills (e.g. mattpocock/skills/skills/)
// and registers them into the Orchestrator registry.
func (a *ExternalSkillAdapter) ImportSkillDirectory(rootPath string) (int, error) {
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

		skillID := "ext_" + strings.ReplaceAll(strings.ToLower(meta.Name), " ", "_")
		skillID = strings.ReplaceAll(skillID, "-", "_")

		// 1. Register as native Skill
		skillDir := filepath.Dir(path)
		skill := &config.Skill{
			ID:          skillID,
			Name:        meta.Name,
			Description: meta.Description,
			Capability:  "external_discipline",
			Domain:      "EXTERNAL",
			TokenEstimate: 500,
			ExecutionMode: config.ExecutionModeDirectory,
			SkillDir:      skillDir,
			Provides:      meta.Provides,
			Manifest:      scanSkillDir(skillDir),
		}

		a.Registry.Mu.Lock()
		if a.Registry.Skills == nil {
			a.Registry.Skills = make(map[string]*config.Skill)
		}
		a.Registry.Skills[skillID] = skill

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

	name := filepath.Base(filepath.Dir(path))
	description := ""
	var provides []string

	for _, fl := range frontmatterLines {
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
			}
		}
	}

	return &ExternalSkillMeta{
		Name:        name,
		Description: description,
		Body:        strings.Join(bodyLines, "\n"),
		Provides:    provides,
	}, nil
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
