package core

import (
	"log"

	"github.com/daybeam/vortex/core/obs"
)

// Wait blocks until reflection's tracked background work finishes.
func (re *ReflectionEngine) Wait() {
	re.wg.Wait()
}

func (re *ReflectionEngine) logObservation(event, taskID, reason string) {
	detail := map[string]any{"reason": reason, "task_id": taskID}
	if re != nil && re.logger != nil {
		re.logger.Log(obs.EventType(event), taskID, "", detail)
		return
	}
	log.Printf("%s task=%s reason=%s", event, taskID, reason)
}
