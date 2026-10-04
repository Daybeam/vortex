package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/daybeam/vortex/schemas"
)

// writeIfAbsent writes data to path only if the file does not already exist.
// Returns true if it wrote (file was absent), false if it skipped (file existed).
func writeIfAbsent(path string, data []byte) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return false, err
	}
	return true, nil
}

// SeedBuiltins writes built-in roles, SOPs, and skills to disk if they don't
// already exist. Called once at startup after NewRegistry and before
// BootstrapConfigSync. Existing files are never overwritten.
func (r *Registry) SeedBuiltins(rolesDir, sopsDir, skillsDir string) error {
	if err := r.seedBuiltinRoles(rolesDir); err != nil {
		return fmt.Errorf("seed roles: %w", err)
	}
	if err := r.seedBuiltinSOPs(sopsDir); err != nil {
		return fmt.Errorf("seed sops: %w", err)
	}
	if err := r.seedBuiltinSkills(skillsDir); err != nil {
		return fmt.Errorf("seed skills: %w", err)
	}
	return nil
}

// ── Built-in Role Prompts (centralized constants) ────────────────────────
// These are the compiled-in defaults. External overrides live in
// workspace/prompts/roles/<role_id>.txt and take priority when present
// (see PromptOverrides.RoleInstructions).

const sopGeneratorInstruction = `You are the SOP Generator. Given a task description, explore the available MCP tools, design a workflow DAG, and produce a valid SOP JSON. Submit it via the update_sop tool.

Guidelines:
1. First, enumerate all available tools and their parameter schemas.
2. Identify which tools are needed for the task.
3. Design a step-by-step DAG with clear dependencies and exit criteria.
4. Assign appropriate roles to each step.
5. Output the SOP as valid JSON and submit via update_sop.`

// ── Built-in Roles ────────────────────────────────────────────────────────

var builtinRoles = []Role{
	{
		ID:             "sop_generator",
		Name:           "SOP Generator",
		BaseCapability: "plan",
		Instruction:    sopGeneratorInstruction,
		AllowDynamicSkills: true,
		AllowDynamicMCPs:   true,
		Generatable:        true,
	},
}

func (r *Registry) seedBuiltinRoles(rolesDir string) error {
	for _, role := range builtinRoles {
		if override, ok := r.PromptOverrides.RoleInstructions[role.ID]; ok && override != "" {
			role.Instruction = override
		}
		path := filepath.Join(rolesDir, role.ID+".json")
		data, err := json.MarshalIndent(role, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal role %s: %w", role.ID, err)
		}
		wrote, err := writeIfAbsent(path, data)
		if err != nil {
			return fmt.Errorf("write role %s: %w", role.ID, err)
		}
		if wrote {
			log.Printf("[seed] role %s → %s", role.ID, path)
		}
	}
	return nil
}

// ── Built-in SOPs ─────────────────────────────────────────────────────────

var builtinSOPs = []schemas.SOP{
	{
		ID:             "sop_auto_generate",
		Version:        "1.0",
		Author:         "system",
		MutableByAgent: true,
		Description:    "Self-improvement loop: given a task description, explore available MCP tools, generate a SOP, and save it.",
		Tags:           []string{"meta", "self-improvement"},
		TypicalTasks:   []string{"create a new SOP", "automate a workflow"},
		Triggers: []schemas.SOPTrigger{
			{Keywords: []string{"generate sop", "create workflow", "automate task"}},
		},
		Steps: map[string]schemas.SOPStep{
			"explore": {
				ID:    "explore",
				Role:  "sop_generator",
				Task:  "Explore all available MCP tools and their schemas. List each tool name, its parameters, and what it does.",
				Skills: []string{"code_search"},
			},
			"design": {
				ID:       "design",
				Role:     "sop_generator",
				Task:     "Based on the explored tools, design a workflow DAG for the requested task. Define steps, dependencies, roles, and exit criteria.",
				DependsOn: []string{"explore"},
			},
			"save": {
				ID:       "save",
				Role:     "sop_generator",
				Task:     "Convert the designed workflow into a valid SOP JSON and submit it via the update_sop tool.",
				DependsOn: []string{"design"},
				ExitCriteria: "SOP saved successfully and validateSOPDAG passes",
			},
		},
	},
}

func (r *Registry) seedBuiltinSOPs(sopsDir string) error {
	for _, sop := range builtinSOPs {
		path := filepath.Join(sopsDir, sop.ID+".json")
		data, err := json.MarshalIndent(sop, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal sop %s: %w", sop.ID, err)
		}
		wrote, err := writeIfAbsent(path, data)
		if err != nil {
			return fmt.Errorf("write sop %s: %w", sop.ID, err)
		}
		if wrote {
			log.Printf("[seed] sop %s → %s", sop.ID, path)
		}
	}
	return nil
}

// ── Built-in Skills ───────────────────────────────────────────────────────

var builtinSkills = []*Skill{
	{
		ID:          "read_file",
		Name:        "Read File",
		Capability:  "code",
		Description: "Read content of a file from disk.",
		Implementations: map[string]SkillImplementation{
			"default": {
				SystemPrompt: "When you need to see the content of a file, use the available read_file or similar tool. Analyze the content carefully.",
			},
		},
	},
	{
		ID:          "code_search",
		Name:        "Code Search",
		Capability:  "code",
		Description: "Search for symbols or text across the project.",
		Implementations: map[string]SkillImplementation{
			"default": {
				SystemPrompt: "Use code search tools to find relevant files and definitions before making changes.",
			},
		},
	},
	{
		ID:          "sop_writing",
		Name:        "SOP Writing",
		Capability:  "plan",
		Description: "Guidance for generating valid SOP JSON with correct DAG structure, step dependencies, and exit criteria.",
		Implementations: map[string]SkillImplementation{
			"default": {
				SystemPrompt: "When writing a SOP, ensure: (1) each step has a unique ID, (2) depends_on references exist, (3) no cycles in the DAG, (4) exit_criteria are testable, (5) roles are defined in the registry.",
			},
		},
	},
}

func (r *Registry) seedBuiltinSkills(skillsDir string) error {
	for _, skill := range builtinSkills {
		path := filepath.Join(skillsDir, skill.ID+".json")
		data, err := json.MarshalIndent(skill, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal skill %s: %w", skill.ID, err)
		}
		wrote, err := writeIfAbsent(path, data)
		if err != nil {
			return fmt.Errorf("write skill %s: %w", skill.ID, err)
		}
		if wrote {
			log.Printf("[seed] skill %s → %s", skill.ID, path)
		}
	}
	return nil
}
