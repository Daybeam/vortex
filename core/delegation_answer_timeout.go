package core

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Q4-6 (register O20, answered 2026-11-05): 「把"分钟级无回传"显式化为错误，超时值做成
// 可配置，默认给宽」.
//
// O20 measured the ambiguity: an envelope appeared after get_task_status had
// already answered pending=none, and a second submission never came back within
// 12 minutes. A host standing there cannot tell "the delegation is slow" from
// "my submission was lost", because a parked delegation waits forever and the
// status says only "running".
//
// This file turns that silence into a report. It changes no state: past the
// window the status dict carries delegation_wait with an error sentence naming
// the decision, the elapsed wait, and how to answer it. Deliberately NOT an
// abort — O22's lesson is that a delegated step's downstream is forfeited
// silently if anything resolves it on a timer, and O20's own operator error was
// treating a parked-but-unanswered delegation as "nothing to lose" and cancelling
// it. The answer stays reachable; only the noise level changes.

// defaultDelegationAnswerTimeout is the "wide" default Q4-6 asked for. Set from
// the measurements in O20 rather than picked for roundness: envelope compilation
// tops out around 26 s, and a delegated step is a whole sub-agent run, so the
// window has to absorb minutes of legitimate work. It matches approvalExpiry's
// 30 m so one number governs "a human/host has been slow" across both shapes.
const defaultDelegationAnswerTimeout = 30 * time.Minute

// StaleDelegation is one parked delegation whose answer has not arrived.
type StaleDelegation struct {
	DecisionID     string `json:"decision_id"`
	StepID         string `json:"step_id"`
	WaitingSeconds int    `json:"waiting_seconds"`
}

// delegationAnswerTimeout reads system.delegation_answer_timeout_seconds with
// the approvalExpiry convention: nil ⇒ default, <=0 ⇒ reporting disabled.
func (s *DirectedEngine) delegationAnswerTimeout() time.Duration {
	if s.registry == nil {
		return defaultDelegationAnswerTimeout
	}
	t := s.registry.System.DelegationAnswerTimeoutSeconds
	if t == nil {
		return defaultDelegationAnswerTimeout
	}
	if *t <= 0 {
		return 0
	}
	return time.Duration(*t) * time.Second
}

// staleDelegationsLocked returns the delegation decisions parked longer than
// timeout. Caller must hold s.Mu (read lock is enough). A zero Created is
// skipped: an answer can only be late relative to a timestamp that exists, and
// graphs rebuilt from a manifest written before Created existed would otherwise
// look months stale.
func (s *DirectedEngine) staleDelegationsLocked(graph *schemas.TaskGraph, now time.Time, timeout time.Duration) []StaleDelegation {
	if timeout <= 0 || graph == nil {
		return nil
	}
	var stale []StaleDelegation
	for _, dec := range graph.PendingDecisions {
		if dec == nil || dec.Type != schemas.DecisionDelegationRequired || dec.Created.IsZero() {
			continue
		}
		waited := now.Sub(dec.Created)
		if waited < timeout {
			continue
		}
		stale = append(stale, StaleDelegation{
			DecisionID:     dec.ID,
			StepID:         dec.StepID,
			WaitingSeconds: int(waited.Seconds()),
		})
	}
	return stale
}

// delegationAnswerError words the report as an error a host can act on. It names
// what is still reachable so nobody reads it as "already lost" — including the
// warning that cancel destroys the parked envelope (O20's operator error).
func delegationAnswerError(stale []StaleDelegation, timeout time.Duration) string {
	oldest := stale[0]
	for _, d := range stale[1:] {
		if d.WaitingSeconds > oldest.WaitingSeconds {
			oldest = d
		}
	}
	limit := int(timeout.Seconds())
	lead := fmt.Sprintf("no answer for delegation %q (step %q) after %ds, past the %ds limit",
		oldest.DecisionID, oldest.StepID, oldest.WaitingSeconds, limit)
	if len(stale) > 1 {
		lead = fmt.Sprintf("no answer for %d parked delegations, oldest %q (step %q) at %ds, past the %ds limit",
			len(stale), oldest.DecisionID, oldest.StepID, oldest.WaitingSeconds, limit)
	}
	return lead + ": the step is still parked and nothing has been forfeited -- this is a report, not an abort. " +
		"Answer it with orchestrator_submit_decision (choice=fulfill plus the result payload, or skip|abort) " +
		"or leave it parked; cancel_task destroys the envelope the host was given. " +
		fmt.Sprintf("Raise system.delegation_answer_timeout_seconds (now %ds) if that window is too tight.", limit)
}

// attachDelegationWaitLocked adds the no-answer report to a status dict. It is
// called while the caller holds s.Mu, from both the status and wait paths, so
// the cheap summary view a polling host reads is the one that carries it.
func (s *DirectedEngine) attachDelegationWaitLocked(status map[string]any, graph *schemas.TaskGraph, now time.Time) {
	timeout := s.delegationAnswerTimeout()
	stale := s.staleDelegationsLocked(graph, now, timeout)
	if len(stale) == 0 {
		return
	}
	status["delegation_wait"] = map[string]any{
		"stale":           true,
		"timeout_seconds": int(timeout.Seconds()),
		"decisions":       stale,
		"error":           delegationAnswerError(stale, timeout),
	}
	status["next_action"] = "submit_decision"
}
