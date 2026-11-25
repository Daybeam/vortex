package api

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
)

// Scheduled task delivery to a chat session — Phase 2 of
// SCHEDULE_DELIVERY_TARGETS_DESIGN.md.
//
// When a schedule has Deliver.Mode = "session_current", the ScheduleManager
// calls DeliverToSession after the task completes. This starts a wake turn
// in the session that created the schedule, so the model can read the
// artifacts and report the result to the user.
//
// The wake uses reason = "cron:<scheduleID>:<slot>" which:
//   - bypasses the session-level maxFollowUpDepth cap (per-job quota is
//     enforced by Schedule.MaxRuns, not the session depth counter)
//   - avoids dedup collision with regular "task_done" wakes
//
// See SCHEDULE_DELIVERY_TARGETS_DESIGN.md §3 (quota) and §5 (dispatch).

// DeliverToSession implements core.SessionDeliverer. It delivers a finished
// scheduled task's result to the chat session that created the schedule.
func (h *ChatHandler) DeliverToSession(ctx context.Context, sessionID, taskID, scheduleID string, result map[string]any) error {
	if h == nil || h.Harness == nil || h.Store == nil || sessionID == "" || taskID == "" {
		return core.ErrSessionUnavailable
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		return core.ErrSessionMissing
	}
	if expectedOwner, _ := result["delivery_owner"].(string); expectedOwner != "" && session.GetOwnerID() != "" && session.GetOwnerID() != expectedOwner {
		return core.ErrSessionMissing
	}

	taskDesc, _ := result["task"].(string)
	if taskDesc == "" {
		taskDesc = taskID
	}
	status, _ := result["status"].(string)
	evType := string(obs.EventTaskFailed)
	if status == "completed" || status == "completed_with_skips" {
		evType = string(obs.EventTaskCompleted)
	}

	outcome := collectTaskOutcome(taskID, taskDesc, evType, result)
	summary := outcome.summary()

	reason := fmt.Sprintf("cron:%s:%s", scheduleID, taskID)

	text := fmt.Sprintf(followUpTriggerFmt, taskID)
	if summary != "" {
		text += "\n\n" + summary
	}

	seed := wakeSeed{
		digest:    wakeArtifactDigest(outcome),
		fallback:  wakeFallbackReport(outcome),
		declared:  outcome.Declared,
		contracts: outcome.Contracts,
		figures:   headlineFigures(outcome.WorkspaceRoot, outcome.Declared, defaultMinSigDigits),
	}

	switch h.startFollowUpTurnWithTriggerSeed(sessionID, taskID, reason, text, seed) {
	case followUpStarted:
		return nil
	case followUpBusy:
		h.enqueueWake(sessionID, pendingWakeEntry{
			taskID:    taskID,
			reason:    reason,
			text:      text,
			digest:    seed.digest,
			fallback:  seed.fallback,
			declared:  seed.declared,
			figures:   seed.figures,
			contracts: seed.contracts,
		})
		return core.ErrSessionBusy
	case followUpSuppressed:
		return nil
	default:
		return core.ErrSessionUnavailable
	}
}
