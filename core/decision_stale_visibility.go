package core

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// stalePendingDecisionsLocked applies each decision's existing reporting window:
// delegation_answer_timeout_seconds for delegations, decision_stale_timeout_seconds
// for every other type. It only reads state; no decision is expired or resolved.
func (s *DirectedEngine) stalePendingDecisionsLocked(graph *schemas.TaskGraph, now time.Time) []StaleDecision {
	if graph == nil {
		return nil
	}
	delegationTimeout := s.delegationAnswerTimeout()
	genericTimeout := s.decisionStaleTimeout()
	var stale []StaleDecision
	for _, dec := range graph.PendingDecisions {
		if dec == nil || dec.Created.IsZero() {
			continue
		}
		timeout := genericTimeout
		if dec.Type == schemas.DecisionDelegationRequired {
			timeout = delegationTimeout
		}
		if timeout <= 0 {
			continue
		}
		waited := now.Sub(dec.Created)
		if waited < timeout {
			continue
		}
		stale = append(stale, StaleDecision{
			DecisionID:     dec.ID,
			StepID:         dec.StepID,
			Type:           dec.Type,
			TimeoutSeconds: int(timeout.Seconds()),
			WaitingSeconds: int(waited.Seconds()),
		})
	}
	return stale
}

func decisionStaleError(stale []StaleDecision) string {
	oldest := stale[0]
	for _, d := range stale[1:] {
		if d.WaitingSeconds > oldest.WaitingSeconds {
			oldest = d
		}
	}
	return fmt.Sprintf("%d pending decisions are stale; oldest %q (step %q, type %q) has waited %ds, past its %ds reporting window: this is a report, not an abort, and every decision remains answerable with orchestrator_submit_decision",
		len(stale), oldest.DecisionID, oldest.StepID, oldest.Type, oldest.WaitingSeconds, oldest.TimeoutSeconds)
}

func (s *DirectedEngine) attachDecisionStaleLocked(status map[string]any, graph *schemas.TaskGraph, now time.Time) {
	stale := s.stalePendingDecisionsLocked(graph, now)
	if len(stale) == 0 {
		return
	}
	status["decision_stale"] = map[string]any{
		"stale":               true,
		"oldest_wait_seconds": oldestWaitSeconds(stale),
		"decisions":           stale,
		"error":               decisionStaleError(stale),
	}
	if _, exists := status["next_action"]; !exists {
		status["next_action"] = "submit_decision"
	}
}
