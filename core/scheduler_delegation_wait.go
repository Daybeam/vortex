package core

import (
	"context"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_delegation_wait.go — D1 fix (DELEGATION_HANDOVER_REDESIGN.md §4.1).
//
// Problem: submit_task only returns task_id. In delegation mode the host needs
// the prompt envelope (system_prompt, user_prompt, tool_definitions) to execute
// the step itself, but can only get it via a separate get_task_status(view=full)
// poll — a blind 2N-round-trip handshake.
//
// Fix: FirstDelegations lets the submit handler synchronously wait for the
// first batch of delegation decisions and return their envelopes in the same
// response, reducing the handshake to N+1 round-trips.

// DelegationView is a read-only projection of a delegation decision, suitable
// for returning to the host in submit_task / submit_decision responses.
// All fields are additive (I6): old hosts that ignore "delegations" behave
// exactly as before.
type DelegationView struct {
	StepID     string         `json:"step_id"`
	DecisionID string         `json:"decision_id"`
	Kind       string         `json:"kind"`
	Options    []string       `json:"options"`
	Prompt     map[string]any `json:"prompt,omitempty"`
	RoleID     string         `json:"role_id,omitempty"`
}

// FirstDelegations waits up to timeout for the task to produce delegation
// decisions, returning their read-only views. Returns nil,false if no
// delegation decisions appear within the timeout or if the context is cancelled.
//
// This covers both the single-step fast-path (decision produced in a goroutine
// via scheduler_submit.go:597) and the multi-step walker (decision produced in
// executeStep). Engine-side polling is the minimal "one place covers both paths"
// approach — see design doc §4.1 for the rationale vs. changing Submit's signature.
func (s *DirectedEngine) FirstDelegations(ctx context.Context, taskID string, timeout time.Duration) ([]DelegationView, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		views := s.snapshotDelegationViews(taskID)
		if len(views) > 0 {
			return views, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// snapshotDelegationViews collects all pending delegation decisions for a task
// under a brief RLock, returning their read-only projections.
func (s *DirectedEngine) snapshotDelegationViews(taskID string) []DelegationView {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	graph, ok := s.graphs[taskID]
	if !ok {
		return nil
	}

	var views []DelegationView
	for _, dec := range graph.PendingDecisions {
		if dec.Type != schemas.DecisionDelegationRequired {
			continue
		}
		kind, _ := dec.Context["kind"].(string)
		prompt, _ := dec.Context["prompt"].(map[string]any)
		roleID, _ := dec.Context["role_id"].(string)
		views = append(views, DelegationView{
			StepID:     dec.StepID,
			DecisionID: dec.ID,
			Kind:       kind,
			Options:    dec.Options,
			Prompt:     prompt,
			RoleID:     roleID,
		})
	}
	return views
}
