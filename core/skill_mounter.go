package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/config"
)

// ActiveSkillDirKey is the context key for the active skill's directory.
// Tools (read_file, terminal) can use this to resolve relative paths.
type ActiveSkillDirKey struct{}

// SkillMounter handles activation of discovered skills: injecting their
// description + manifest summary into the prompt and resolving relative paths.
type SkillMounter struct {
	ActiveSkillID  string
	ActiveSkillDir string
}

// Activate assembles a prompt fragment from the skill's description and manifest.
// Returns the fragment string; the caller appends it to the system prompt or task.
func (m *SkillMounter) Activate(skill *config.Skill) string {
	m.ActiveSkillID = skill.ID
	m.ActiveSkillDir = skill.SkillDir

	var sb strings.Builder
	sb.WriteString(skill.Description)

	if skill.Manifest != nil &&
		(len(skill.Manifest.References) > 0 ||
			len(skill.Manifest.Scripts) > 0 ||
			len(skill.Manifest.Assets) > 0) {
		sb.WriteString("\n\n## Available Resources (relative to skill dir)\n")
		if len(skill.Manifest.References) > 0 {
			sb.WriteString(fmt.Sprintf("- References: %s\n",
				strings.Join(skill.Manifest.References, ", ")))
		}
		if len(skill.Manifest.Scripts) > 0 {
			sb.WriteString(fmt.Sprintf("- Scripts: %s\n",
				strings.Join(skill.Manifest.Scripts, ", ")))
		}
		if len(skill.Manifest.Assets) > 0 {
			sb.WriteString(fmt.Sprintf("- Assets: %s\n",
				strings.Join(skill.Manifest.Assets, ", ")))
		}
		sb.WriteString("\nUse relative paths in read_file / terminal to access them.\n")
	}

	// agentskills.io standard fields (docs/plan/AGENTIC_SKILL_EXPLORATION_DESIGN.md G2/G3):
	// compatibility is surfaced as a warning the agent verifies itself (no
	// blocking check); allowed-tools is advisory only — actual tool filtering
	// remains the role-binding allowlist's job.
	if skill.Compatibility != "" {
		sb.WriteString("\n\n## Compatibility Requirements (verify before using scripts)\n")
		sb.WriteString(skill.Compatibility)
		sb.WriteString("\n")
	}
	if len(skill.AllowedTools) > 0 {
		sb.WriteString("\n\n## Pre-approved Tools\n")
		sb.WriteString(strings.Join(skill.AllowedTools, " "))
		sb.WriteString("\n")
	}

	// L2 body injection (G1): when a directory-mode skill matches the task, load
	// the SKILL.md instructions into the prompt (progressive disclosure level 2).
	// Read from disk on demand — bodies are NOT stored in the registry, keeping
	// L1 metadata cheap. Hard-capped to keep oversized skills from overflowing
	// the context (regression tests pinned this concern before L2 existed).
	if skill.ExecutionMode == config.ExecutionModeDirectory && skill.SkillDir != "" {
		if body := readSkillBody(skill.SkillDir); body != "" {
			sb.WriteString("\n\n## Skill Instructions (from SKILL.md)\n")
			sb.WriteString(body)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// skillBodyPromptCap bounds the SKILL.md body injected into a prompt fragment.
// agentskills.io recommends instructions stay under ~5000 tokens; 4000 chars is
// a conservative budget that leaves room for the top-N activation multiplicity
// (ActivateTopSkills mounts up to 3 skills per task).
const skillBodyPromptCap = 4000

// readSkillBody loads SKILL.md from the skill directory, strips frontmatter
// (reusing skill_interpreter.go's stripFrontmatter), and truncates to
// skillBodyPromptCap with a pointer to the full file. Returns "" on any
// failure or empty body — activation degrades to description-only.
func readSkillBody(skillDir string) string {
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return ""
	}
	body := strings.TrimSpace(stripFrontmatter(string(data)))
	if body == "" {
		return ""
	}
	if len(body) > skillBodyPromptCap {
		body = strings.ToValidUTF8(body[:skillBodyPromptCap], "") +
			"\n\n[... truncated — read SKILL.md in the skill directory for the remaining instructions ...]"
	}
	return body
}

// ResolvePath joins a relative path with the active skill's directory.
// Absolute paths are passed through unchanged.
func (m *SkillMounter) ResolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if m.ActiveSkillDir == "" {
		return p
	}
	return filepath.Join(m.ActiveSkillDir, p)
}

// ActivateTopSkills runs the SkillMounter over the top-N skill candidates
// from discovery results and returns the concatenated prompt fragment.
// It looks up skills from the registry by candidate ID.
func ActivateTopSkills(reg *config.Registry, candidates []DiscoveryCandidate, topN int) string {
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	var sb strings.Builder
	count := 0
	for _, c := range candidates {
		if c.Type != CandidateSkill || count >= topN {
			continue
		}
		sk, ok := reg.Skills[c.ID]
		if !ok {
			continue
		}
		mounter := &SkillMounter{}
		fragment := mounter.Activate(sk)
		if fragment == "" {
			continue
		}
		if count > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(fragment)
		count++
	}

	if count == 0 {
		return ""
	}
	return "\n\n## Active Skills\n" + sb.String()
}
