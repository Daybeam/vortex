package chat

import (
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// checkPlanDrift checks the first tool call in a batch against the step plan.
// If drift is detected, sets PendingSteer. Extracted from Run for Q6-1 ratchet.
func (h *ChatHarness) checkPlanDrift(i int, taskID string, tc schemas.ToolCall, emit func(ChatEvent) error) {
	if i != 0 || h.Sieve == nil || h.PendingSteer != "" {
		return
	}
	if msg := h.Sieve.DetectPlanDrift(taskID, tc); msg != "" {
		h.SteerLock.Lock()
		h.PendingSteer = msg
		h.SteerLock.Unlock()
		emit(ChatEvent{Type: "plan_drift", Data: tc.Name})
	}
}

// checkDelegationDrift counts read-only exploratory tool calls per turn and
// injects a delegation steer when the threshold is reached. Extracted from Run
// to satisfy the Q6-1 size ratchet (Run may only shrink).
// See docs/completed/2026-11-08/CHAT_HARNESS_DELEGATION_DRIFT_DESIGN.md.
func (h *ChatHarness) checkDelegationDrift(tc schemas.ToolCall, emit func(ChatEvent) error) {
	if tc.Name != "read_file" && tc.Name != "grep" && tc.Name != "find" {
		return
	}
	h.fileReadCalls++
	if h.fileReadCalls < 6 || h.fileReadCalls >= 15 || h.delegationSteered {
		return
	}
	h.SteerLock.Lock()
	defer h.SteerLock.Unlock()
	if h.PendingSteer == "" {
		h.PendingSteer = fmt.Sprintf(
			"[DELEGATION DRIFT] You have called file-reading/grep tools %d times. "+
				"If this task spans multiple files, sources, or sub-questions (see WHEN TO DELEGATE), "+
				"consider using delegate_to_orchestrator. If you are already finishing up, answer directly.",
			h.fileReadCalls)
		h.delegationSteered = true
		emit(ChatEvent{Type: "delegation_drift", Data: tc.Name})
	}
}
