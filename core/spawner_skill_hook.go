package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
)

// buildSkillMetadata generates a lightweight metadata block for skills,
// especially for DirectorySkills to avoid context bloat.
func (s *Spawner) buildSkillMetadata(hub *ContextHub, skillIDs []string, model, family string) (string, error) {
	var sb strings.Builder
	for _, id := range skillIDs {
		skill := hub.GetSkill(id)
		if skill == nil {
			continue
		}

		if skill.ExecutionMode == config.ExecutionModeDirectory {
			// DirectorySkill: Inject only metadata and execution framework hint.
			// The actual logic is expanded into separate steps by the scheduler.
			sb.WriteString(fmt.Sprintf("# Skill: %s (%s)\n", skill.Name, skill.ID))
			sb.WriteString(fmt.Sprintf("Description: %s\n", skill.Description))
			sb.WriteString("Status: [Directory-Mode] Executing multi-step declarative DAG. Refer to the current step task for detailed instructions.\n\n")
		} else {
			// FlatSkill: Inject full prompt (legacy behavior)
			prompt, err := skill.GetPrompt(model, family)
			if err != nil {
				sb.WriteString(fmt.Sprintf("# Skill %s\n%v\n\n", id, err))
			} else {
				sb.WriteString(prompt + "\n\n")
			}
		}
	}
	return sb.String(), nil
}
