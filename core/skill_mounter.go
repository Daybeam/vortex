package core

import (
	"fmt"
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

	return sb.String()
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
