package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/schemas"
)

// O16 regression (2026-11-04, 实测 twice in the register): in delegation mode the
// Spawner never calls a provider — it returns the assembled prompt with
// StatusDelegationRequired (core/spawner_utils.go:1095) — and the DAG loop
// flattened that map with %v and ran the output-loop Guardian over it. Every
// veto destroyed a hand-over the host was never going to see, so task_f31f1868
// produced four vetoes and zero envelopes, and task_e3e78311 lost the hunter
// step the same way. The exemption has to cover *all* hand-overs, not only the
// ones carrying the mounted-skill index, because the single-skill rendering
// trades the "---" trip for the whole skill body (O21).

// stuckText is text the Guardian must reject when it really is an output.
func stuckText() string {
	return strings.Repeat("I will now read the file and report the findings.\n", 20)
}

func TestGuardStepOutput_DelegationHandoverIsNeverVetoed(t *testing.T) {
	s := &DirectedEngine{sieve: sieve.NewSieve(10)}

	handover := &SpawnResult{Output: schemas.SubagentOutput{Status: schemas.StatusDelegationRequired}}
	// requiredSchema is "json" for a step with ExitCriteria == "json"
	// (core/scheduler_dag_exec.go:607-609), which is the shape both recorded
	// incidents had.
	if ok, reason := s.guardStepOutput(context.Background(), handover, stuckText(), "json"); !ok {
		t.Errorf("hand-over vetoed with %q; an assembled input is not an output that can be in a logic loop", reason)
	}

	// The same text from a real provider answer is still inspected — the
	// exemption must not become a way to switch the Guardian off.
	normal := &SpawnResult{Output: schemas.SubagentOutput{Status: schemas.StatusOK}}
	if ok, reason := s.guardStepOutput(context.Background(), normal, stuckText(), ""); ok {
		t.Error("a stuck provider output passed the Guardian, so the exemption disabled the check")
	} else if !strings.Contains(reason, "logic loop") {
		t.Errorf("reason = %q, want the repetition verdict to survive", reason)
	}

	// Partial and failed answers are outputs too and must not inherit the pass.
	for _, st := range []schemas.SubagentStatus{schemas.StatusPartial, schemas.StatusFailed} {
		if ok, _ := s.guardStepOutput(context.Background(), &SpawnResult{Output: schemas.SubagentOutput{Status: st}}, stuckText(), ""); ok {
			t.Errorf("status %q was exempted as if it were a hand-over", st)
		}
	}
}
