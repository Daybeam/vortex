package core

import "github.com/daybeam/vortex/schemas"

// PendingDecisionType reports the type of a parked decision, looked up by its
// ID, and whether the lookup succeeded.
//
// Q4-5 (2026-11-05, operator ruling: 「按 decision_id 路由，同时保留按载荷存在与否
// 路由作兜底」): orchestrator_submit_decision used to pick its engine entry point
// from the mere presence of an `output` payload, so a host that serialised an
// empty result object for a skip|abort landed in the delegation-only
// FulfillStep (O17). The decision's own type is the fact that determines what
// an answer may carry, so the tool layer asks for it here and keeps the payload
// rule only as the fallback for ids it cannot resolve.
//
// ok=false means "task unknown or decision not parked" — the caller must fall
// back to the payload rule, not guess. This is a read, not a second gate: an
// answer that still reaches FulfillStep for a non-delegation decision gets the
// same misroutedFulfillError the engine has always returned.
func (s *DirectedEngine) PendingDecisionType(taskID, decisionID string) (schemas.DecisionType, bool) {
	// Write lock, not RLock: getOrReloadGraphLocked inserts into s.graphs on a
	// cache miss (caller contract in scheduler_status.go).
	s.Mu.Lock()
	defer s.Mu.Unlock()
	graph := s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		return "", false
	}
	dec, _ := findPendingDecisionLocked(graph, decisionID)
	if dec == nil {
		return "", false
	}
	return dec.Type, true
}
