package core

import "log"

func (s *PromptAssembler) logMemoryBankSkip(taskID, reason string) {
	detail := map[string]any{"reason": reason, "task_id": taskID}
	if s.logger != nil {
		s.logger.Log("memorybank_injection_skipped", taskID, "", detail)
		return
	}
	log.Printf("memorybank_injection_skipped task=%s reason=%s", taskID, reason)
}
