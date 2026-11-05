package core

import (
	"context"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_output_guard.go — the single place that decides whether the Sieve
// Guardian may veto a step result.

// guardStepOutput runs the Guardian over a step's rendered output.
//
// O16/O21 (2026-11-04): a delegation hand-over is not an output. In delegation
// mode the Spawner never calls a provider — it returns the assembled prompt with
// StatusDelegationRequired (core/spawner_utils.go:1095) — and the DAG loop
// flattened it with %v and ran the output-loop detector over it, so the engine
// destroyed its own hand-over before the host could see it (task_f31f1868: four
// vetoes and zero envelopes; task_e3e78311: the hunter step died the same way on
// a JSON output template). "The output entered a logic loop" is not a judgement
// that can ever be right about a payload whose only purpose is to be executed
// somewhere else, so hand-overs skip the inspection instead of being tolerated by
// a threshold.
//
// This lives outside executeStep because that function is already over its frozen
// size baseline (scripts/size_baseline.txt) and the exemption is one condition.
func (s *DirectedEngine) guardStepOutput(ctx context.Context, result *SpawnResult, outputText, requiredSchema string) (bool, string) {
	if result != nil && result.Output.Status == schemas.StatusDelegationRequired {
		return true, ""
	}
	return s.sieve.InspectWithSystemOne(ctx, outputText, requiredSchema)
}
