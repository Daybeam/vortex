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
// into Vortex's native config.Skill and SOP registries.
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
}

// ImportSkillDirectory scans a directory of skills (e.g. mattpocock/skills/skills/)
// and registers them into the Vortex registry.
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
		skill := &config.Skill{
			ID:            skillID,
			Name:          meta.Name,
			Description:   meta.Description,
			Capability:    "external_discipline",
			Domain:        "EXTERNAL",
			TokenEstimate: 500,
			ExecutionMode: config.ExecutionModeDirectory,
			SkillDir:      filepath.Dir(path),
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
			}
		}
	}

	return &ExternalSkillMeta{
		Name:        name,
		Description: description,
		Body:        strings.Join(bodyLines, "\n"),
	}, nil
}
