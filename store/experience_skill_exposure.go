package store

// maxSkillExposureKeys bounds the pending-exposure map. Steps that never reach
// RecordTaskCompletion (abandoned/cancelled tasks) would otherwise accumulate
// entries without bound.
const maxSkillExposureKeys = 4096

// RecordSkillExposure remembers generated skills that were attached to a
// step's prompt. Retrieval happens during spawn preparation, while skill
// usage is credited from StepRecord.Skills at task completion; the step never
// carries the retrieved IDs itself, so this handoff is what lets a JIT skill
// ever accrue UsageCount and reach maybePromoteSkill's gate.
func (es *ExperienceStore) RecordSkillExposure(taskID, stepID string, ids []string) {
	if es == nil || stepID == "" || len(ids) == 0 {
		return
	}
	es.Mu.Lock()
	defer es.Mu.Unlock()
	if es.skillExposures == nil {
		es.skillExposures = make(map[string][]string)
	}
	key := taskID + "/" + stepID
	es.skillExposures[key] = append(es.skillExposures[key], ids...)
	for len(es.skillExposures) > maxSkillExposureKeys {
		for k := range es.skillExposures {
			delete(es.skillExposures, k)
			break
		}
	}
}

// TakeSkillExposures returns and clears the skills recorded for a step, so a
// resumed or re-recorded task cannot credit the same exposure twice.
func (es *ExperienceStore) TakeSkillExposures(taskID, stepID string) []string {
	if es == nil || stepID == "" {
		return nil
	}
	es.Mu.Lock()
	defer es.Mu.Unlock()
	key := taskID + "/" + stepID
	ids := es.skillExposures[key]
	if len(ids) > 0 {
		delete(es.skillExposures, key)
	}
	return ids
}
