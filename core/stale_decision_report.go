package core

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Q5-5 (answered 2026-11-06): 「step_failed 等人裁决的 24 小时判"已陈旧"… 到期只新增
// 一个信号，在 /tasks 和 get_task_status 里显示，不自动 abort」.
//
// O22 is why this is a report and not a timer: a step_failed decision parked
// for a day is still the only handle on a step that failed, and resolving it
// automatically forfeits everything downstream. So past the window the status
// dict carries stale_decisions with an error sentence naming the decision, its
// age, and the fact that nothing has been forfeited. State is never touched.
//
// The delegation half of Q5-5 (30 minutes, "suspected dead") lives in
// delegation_answer_timeout.go (Q4-6); this file is the step_failed 24-hour half.

// defaultDecisionStaleTimeout is the operator's number: a step_failed decision
// that has waited a day was already not going to be answered by an idle human,
// but a working day is long enough that 24h does not fire on "left it overnight".
const defaultDecisionStaleTimeout = 24 * time.Hour

// decisionStaleTimeout reads system.decision_stale_timeout_seconds with the same
// convention as delegationAnswerTimeout: nil ⇒ default, <=0 ⇒ reporting off.
func (s *DirectedEngine) decisionStaleTimeout() time.Duration {
	if s.registry == nil {
		return defaultDecisionStaleTimeout
	}
	t := s.registry.System.DecisionStaleTimeoutSeconds
	if t == nil {
		return defaultDecisionStaleTimeout
	}
	if *t <= 0 {
		return 0
	}
	return time.Duration(*t) * time.Second
}

// staleFailedDecisionsLocked returns the step_failed decisions parked longer
// than timeout. Caller must hold s.Mu (read lock is enough). Zero Created is
// skipped for the same reason as the delegation report: a decision restored
// from a manifest written before Created existed has no timestamp to be late
// against. Delegation decisions are deliberately excluded — they have their own
// 30-minute report, and double-reporting one decision under two windows would
// make the two numbers indistinguishable at the status surface.
func (s *DirectedEngine) staleFailedDecisionsLocked(graph *schemas.TaskGraph, now time.Time, timeout time.Duration) []StaleDecision {
	if timeout <= 0 || graph == nil {
		return nil
	}
	var stale []StaleDecision
	for _, dec := range graph.PendingDecisions {
		if dec == nil || dec.Type != schemas.DecisionStepFailed || dec.Created.IsZero() {
			continue
		}
		waited := now.Sub(dec.Created)
		if waited < timeout {
			continue
		}
		stale = append(stale, StaleDecision{
			DecisionID:     dec.ID,
			StepID:         dec.StepID,
			WaitingSeconds: int(waited.Seconds()),
		})
	}
	return stale
}

// staleDecisionError words the report so a reader cannot mistake it for an
// abort or for an already-resolved failure.
func staleDecisionError(stale []StaleDecision, timeout time.Duration) string {
	oldest := stale[0]
	for _, d := range stale[1:] {
		if d.WaitingSeconds > oldest.WaitingSeconds {
			oldest = d
		}
	}
	limit := int(timeout.Seconds())
	lead := fmt.Sprintf("step %q has waited %ds for a decision on its failure (decision %q), past the %ds limit",
		oldest.StepID, oldest.WaitingSeconds, oldest.DecisionID, limit)
	if len(stale) > 1 {
		lead = fmt.Sprintf("%d failed steps have waited for a decision, oldest %q (decision %q) at %ds, past the %ds limit",
			len(stale), oldest.StepID, oldest.DecisionID, oldest.WaitingSeconds, limit)
	}
	return lead + ": the step stays failed and its downstream stays blocked -- this is a report, not an abort, " +
		"nothing has been forfeited. Answer it with orchestrator_submit_decision (retry|skip|abort) or leave it parked. " +
		fmt.Sprintf("Raise system.decision_stale_timeout_seconds (now %ds) if that window is too tight.", limit)
}

// attachStaleDecisionReportLocked adds the stale-failure report to a status
// dict. Called while the caller holds s.Mu, from GetStatus, WaitTask and
// ListGraphs, so every surface a host reads carries the same signal.
func (s *DirectedEngine) attachStaleDecisionReportLocked(status map[string]any, graph *schemas.TaskGraph, now time.Time) {
	timeout := s.decisionStaleTimeout()
	stale := s.staleFailedDecisionsLocked(graph, now, timeout)
	if len(stale) == 0 {
		return
	}
	status["stale_decisions"] = map[string]any{
		"stale":           true,
		"timeout_seconds": int(timeout.Seconds()),
		// Scalar copy for the /tasks list; see attachDelegationWaitLocked.
		"oldest_wait_seconds": oldestWaitSeconds(stale),
		"decisions":           stale,
		"error":               staleDecisionError(stale, timeout),
	}
	// A delegation report names the same next action and is attached first;
	// do not overwrite it, both point the host at submit_decision anyway.
	if _, exists := status["next_action"]; !exists {
		status["next_action"] = "submit_decision"
	}
}
