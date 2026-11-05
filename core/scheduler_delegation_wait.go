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
	StepID        string                `json:"step_id"`
	DecisionID    string                `json:"decision_id"`
	Kind          string                `json:"kind"`
	Options       []string              `json:"options"`
	Prompt        map[string]any        `json:"prompt,omitempty"`
	RoleID        string                `json:"role_id,omitempty"`
	ExecutionHint *ExecutionHint        `json:"execution_hint,omitempty"`
	ACPHint       *schemas.ACPStepConfig `json:"acp_hint,omitempty"`
}

// ExecutionHint tells the host how to execute a delegated step.
// See DELEGATION_HANDOVER_REDESIGN.md §12.3 (P1).
//
// Mode is one of:
//   - "local_fs": the host has filesystem access (admin/local tier). Use
//     orchestrator_run_command with cwd = workspace.task_output_dir.
//   - "orchestrator_tool": the host is itself an orchestrator instance and
//     should execute via its own tool surface.
//   - "none": no execution path available (the caller should not have reached
//     delegation — public+no-sandbox is rejected at submit_task).
type ExecutionHint struct {
	Mode    string `json:"mode"`
	Sandbox string `json:"sandbox,omitempty"` // "task_workspace" when sandboxed
	Note    string `json:"note,omitempty"`    // human-readable guidance
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
	// audit P-8-sibling (L1): use NewTimer+Stop to avoid time.After timer
	// leak on each iteration of this busy-wait loop. Matches the pattern in
	// scheduler_dag.go:227 and schedule_manager.go:315.
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		views := s.snapshotDelegationViews(taskID)
		if len(views) > 0 {
			return views, true
		}
		// N3 fix (DELEGATION_HANDOVER_REDESIGN.md §11.1): if the graph is
		// terminal (all steps in a terminal state), no more delegation
		// decisions will ever appear — return immediately instead of
		// waiting out the full timeout. This eliminates the ~500ms white-wait
		// on the final submit_decision response.
		if s.isGraphTerminal(taskID) {
			return nil, false
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		timer.Reset(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			return nil, false
		case <-timer.C:
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
		// D8 lightweight (DELEGATION_HANDOVER_REDESIGN.md §4.8): when the
		// step has ACP config, expose it as acp_hint so the host can forward
		// the step to its own ACP endpoint instead of executing locally.
		var acpHint *schemas.ACPStepConfig
		if step, ok := graph.Steps[dec.StepID]; ok && step.ACPConfig != nil {
			acpHint = step.ACPConfig
		}
		views = append(views, DelegationView{
			StepID:     dec.StepID,
			DecisionID: dec.ID,
			Kind:       kind,
			Options:    dec.Options,
			Prompt:     prompt,
			RoleID:     roleID,
			ACPHint:    acpHint,
		})
	}
	return views
}

// isGraphTerminal reports whether the task's graph has all steps in a terminal
// state (completed/failed/skipped). Used by FirstDelegations to short-circuit
// the wait when no further delegation decisions can ever appear.
func (s *DirectedEngine) isGraphTerminal(taskID string) bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	graph, ok := s.graphs[taskID]
	return ok && graph.IsTerminal()
}
