package core

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ExpandDirectorySkill parses a directory-based skill into a sequence of steps.
//
// Zone 1 boundary note (C9): Step content (prompts) is read from disk and
// frozen into StepInput.Task at submission time. Mid-run SKILL.md patches
// will NOT affect the step sequence or prompt content of an already-submitted
// task. However, skill metadata (Name, Description, Capability) used by the
// prompt assembler's progressive disclosure IS read live from the registry
// via hub.GetSkill() at each step's prompt assembly. If a config hot-reload
// happens between steps, this metadata could drift. This is cosmetic (prompt
// header only) and does not affect task execution semantics.
func ExpandDirectorySkill(skill *config.Skill, userTask string) ([]schemas.StepInput, error) {
	if skill.ExecutionMode != config.ExecutionModeDirectory {
		return nil, nil // Fallback to native behavior
	}

	promptsDir := skill.PromptsDir
	if promptsDir == "" && skill.SkillDir != "" {
		promptsDir = filepath.Join(skill.SkillDir, "prompts")
	}

	if promptsDir == "" {
		return nil, fmt.Errorf("skill-interpreter: falling back, PromptsDir not specified for DirectorySkill %s", skill.ID)
	}

	// Verify promptsDir exists
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("skill-interpreter: falling back, prompts directory missing: %s", promptsDir)
	}

	// 1. Determine step order
	stepNames, err := resolveStepOrder(skill.SkillDir, promptsDir)
	if err != nil {
		return nil, fmt.Errorf("skill-interpreter: falling back due to error: %w", err)
	}

	if len(stepNames) == 0 {
		return nil, fmt.Errorf("skill-interpreter: falling back, no steps found in %s", promptsDir)
	}

	var result []schemas.StepInput
	var prevStepID string

	for _, stepName := range stepNames {
		path := filepath.Join(promptsDir, stepName+".md")
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("skill-interpreter: falling back, failed to read %s: %w", path, err)
		}

		// Strip frontmatter if present
		taskText := stripFrontmatter(string(content))

		stepID := stepName
		// Force step ID validation: alphanumeric, _, -
		if !isValidStepID(stepID) {
			return nil, fmt.Errorf("skill-interpreter: invalid step ID %q in %s", stepID, path)
		}

		stepInput := schemas.StepInput{
			ID:     stepID,
			RoleID: "subagent", // Default subagent role for skill steps
			Task:   taskText,
		}

		if prevStepID != "" {
			stepInput.DependsOn = []string{prevStepID}
		}
		prevStepID = stepID
		result = append(result, stepInput)
	}

	return result, nil
}

func resolveStepOrder(skillDir, promptsDir string) ([]string, error) {
	// 1. Try SKILL.md frontmatter
	if skillDir != "" {
		skillMdPath := filepath.Join(skillDir, "SKILL.md")
		if steps, err := parseStepsFromSkillMd(skillMdPath); err == nil && len(steps) > 0 {
			return steps, nil
		}
	}

	// 2. Default to alphabetical order
	entries, err := os.ReadDir(promptsDir)
	if err != nil {
		return nil, err
	}

	var steps []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			steps = append(steps, strings.TrimSuffix(entry.Name(), ".md"))
		}
	}
	sort.Strings(steps)
	return steps, nil
}

func parseStepsFromSkillMd(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			} else {
				break // End of frontmatter
			}
		}

		if inFrontmatter && strings.HasPrefix(line, "steps:") {
			stepsStr := strings.TrimSpace(strings.TrimPrefix(line, "steps:"))
			// Handle simple [step1, step2] format or just comma separated
			stepsStr = strings.Trim(stepsStr, "[]")
			parts := strings.Split(stepsStr, ",")
			var steps []string
			for _, p := range parts {
				s := strings.Trim(strings.TrimSpace(p), "\"'")
				if s != "" {
					steps = append(steps, s)
				}
			}
			return steps, nil
		}
	}
	return nil, fmt.Errorf("no steps found in frontmatter")
}

func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		return content
	}

	sepCount := 0
	contentStart := 0
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			sepCount++
			if sepCount == 2 {
				contentStart = i + 1
				break
			}
		}
	}

	if sepCount == 2 {
		return strings.Join(lines[contentStart:], "\n")
	}

	return content
}

func isValidStepID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
