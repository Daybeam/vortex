package core

import (
	"log"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// expandSkillsIfNecessary checks if any input targets a role with a DirectorySkill
// and expands it into a sequence of steps.
func (s *DirectedEngine) expandSkillsIfNecessary(inputs []schemas.StepInput, sessionRoles []*config.Role, sessionSkills []*config.Skill) ([]schemas.StepInput, error) {
	if len(inputs) == 0 {
		return inputs, nil
	}

	// For P0, we only expand the first step if it's a DirectorySkill entry point.
	firstInput := inputs[0]

	// Create a temporary hub for resolution
	hub := NewContextHub(s.registry, nil, s.expStore)
	// Inject session IR into hub if provided
	if len(sessionRoles) > 0 || len(sessionSkills) > 0 {
		// We don't have a Graph here yet, so we manually check session lists
	}

	role := hub.GetRole(firstInput.RoleID)
	if role == nil {
		// Check session roles
		for _, r := range sessionRoles {
			if r.ID == firstInput.RoleID {
				role = r
				break
			}
		}
	}

	if role == nil {
		return inputs, nil
	}

	// Find if the role has any bound DirectorySkills
	var dirSkill *config.Skill
	for _, skillID := range role.BoundSkills {
		skill := hub.GetSkill(skillID)
		if skill == nil {
			for _, sk := range sessionSkills {
				if sk.ID == skillID {
					skill = sk
					break
				}
			}
		}

		if skill != nil && skill.ExecutionMode == config.ExecutionModeDirectory {
			dirSkill = skill
			break
		}
	}

	if dirSkill == nil {
		return inputs, nil
	}

	// Expand the skill into a DAG
	expanded, err := ExpandDirectorySkill(dirSkill, firstInput.Task)
	if err != nil {
		log.Printf("[SkillInterpreter] Fallback: %v", err)
		s.logger.Log("EventSkillInterpreterFallback", "", "", map[string]any{
			"skill_id": dirSkill.ID,
			"error":    err.Error(),
		})
		return inputs, nil // Native fallback
	}

	// If expanded, we replace the first input with the expanded sequence.
	// Any subsequent inputs are appended and depend on the last step of the expansion.
	lastExpandedID := expanded[len(expanded)-1].ID

	finalInputs := append([]schemas.StepInput{}, expanded...)

	for i := 1; i < len(inputs); i++ {
		inp := inputs[i]
		if len(inp.DependsOn) == 0 {
			inp.DependsOn = []string{lastExpandedID}
		}
		finalInputs = append(finalInputs, inp)
	}

	return finalInputs, nil
}
