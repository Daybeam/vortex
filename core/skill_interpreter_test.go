package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestExpandDirectorySkill(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup mock skill directory
	skillDir := filepath.Join(tmpDir, "my_skill")
	promptsDir := filepath.Join(skillDir, "prompts")
	os.MkdirAll(promptsDir, 0755)

	// Create mock prompts
	os.WriteFile(filepath.Join(promptsDir, "step1.md"), []byte("Prompt for step 1"), 0644)
	os.WriteFile(filepath.Join(promptsDir, "step2.md"), []byte("---\nsteps: ignored\n---\nPrompt for step 2"), 0644)

	t.Run("Flat Skill - Invariance", func(t *testing.T) {
		skill := &config.Skill{
			ID:            "flat_skill",
			ExecutionMode: config.ExecutionModeFlat,
		}
		steps, err := ExpandDirectorySkill(skill, "test task")
		if err != nil {
			t.Errorf("expected nil error for flat skill, got %v", err)
		}
		if steps != nil {
			t.Errorf("expected nil steps for flat skill, got %v", steps)
		}
	})

	t.Run("Missing Prompts Dir - Fallback", func(t *testing.T) {
		skill := &config.Skill{
			ID:            "missing_dir_skill",
			ExecutionMode: config.ExecutionModeDirectory,
			SkillDir:      filepath.Join(tmpDir, "non_existent"),
		}
		_, err := ExpandDirectorySkill(skill, "test task")
		if err == nil {
			t.Error("expected error for missing prompts dir, got nil")
		}
	})

	t.Run("Valid Directory - Alphabetical DAG", func(t *testing.T) {
		skill := &config.Skill{
			ID:            "dir_skill",
			ExecutionMode: config.ExecutionModeDirectory,
			SkillDir:      skillDir,
		}
		steps, err := ExpandDirectorySkill(skill, "test task")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(steps) != 2 {
			t.Errorf("expected 2 steps, got %d", len(steps))
		}

		if steps[0].ID != "step1" || steps[1].ID != "step2" {
			t.Errorf("unexpected step order or IDs: %s, %s", steps[0].ID, steps[1].ID)
		}

		if steps[0].Task != "Prompt for step 1" {
			t.Errorf("expected 'Prompt for step 1', got %q", steps[0].Task)
		}

		// Check frontmatter stripping
		if steps[1].Task != "Prompt for step 2" {
			t.Errorf("expected 'Prompt for step 2' (stripped), got %q", steps[1].Task)
		}

		// Check chain
		if len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "step1" {
			t.Errorf("expected step2 to depend on step1, got %v", steps[1].DependsOn)
		}
	})

	t.Run("SKILL.md Explicit Order", func(t *testing.T) {
		// Create SKILL.md with explicit order
		skillMdContent := "---\nsteps: [step2, step1]\n---\nMetadata"
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMdContent), 0644)

		skill := &config.Skill{
			ID:            "ordered_skill",
			ExecutionMode: config.ExecutionModeDirectory,
			SkillDir:      skillDir,
		}
		steps, err := ExpandDirectorySkill(skill, "test task")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(steps) != 2 {
			t.Errorf("expected 2 steps, got %d", len(steps))
		}

		if steps[0].ID != "step2" || steps[1].ID != "step1" {
			t.Errorf("expected explicit order (step2, step1), got (%s, %s)", steps[0].ID, steps[1].ID)
		}

		if len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "step2" {
			t.Errorf("expected step1 to depend on step2, got %v", steps[1].DependsOn)
		}
	})
}
