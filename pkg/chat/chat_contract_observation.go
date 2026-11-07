package chat

import (
	"fmt"

	"github.com/daybeam/vortex/core"
)

// contractObservation is the Q0-1 option-B record: the run's final
// completion-contract state, printed, and nothing judged from it.
//
// Option A would have made the engine reject a handover whose artifacts are
// incomplete and spend the parent's retry budget on it. The user declined
// (2026-11-04, "Q0-1 选 B（只观测）"), so this function only measures. It is the
// other half of that same answer: observation needs a declaration channel, which
// is why Q1-1 came first ("A 是 B 的前提").
//
// Why the shape half rides along with present/missing: chat_harness.go's own
// comment on declaredFields records 9 of 35 harness-bench failures whose
// artifacts were present — 5 of them arithmetically correct — and shaped wrongly,
// and existence alone called every one of them done. A metric that reported only
// existence would re-create that false green light in the observability surface.
//
// The parent's retry count is already on the same line for the two paths it
// exists on: contract_defect_steer is the chat-side budget (capped at
// maxContractDefectSteers, and documented as "doubles as the latch and the
// metric"), and the engine-side count is persisted per step as
// schemas.Step.RetryCount (schemas/task.go:225) inside the graph manifest, which
// is why no new counter was added here.
func (h *ChatHarness) contractObservation() string {
	// SessionRoot == "" is the legacy outputs/<taskID> layout: core.MissingArtifacts
	// would resolve the declared bare names against the process working directory,
	// which says nothing about this run (see chat_harness.go:1080-1086). "na" keeps
	// that from reading as present=0, i.e. "the run produced nothing".
	if h.SessionRoot == "" || len(h.declaredArtifacts) == 0 {
		return "contract_present=na contract_missing=na contract_defects=na checks_armed=na"
	}
	present, missing := core.WorkspaceArtifactsComplete(h.SessionRoot, h.declaredArtifacts)
	defects := len(core.WorkspaceArtifactDefects(h.SessionRoot, h.declaredFields))
	// checks_armed, not "checks passed". `contract_defects` above is the TYPE
	// layer only: it counts "the file parses", never "the file does what the task
	// asked", so it is 0 on a run whose SQL cannot execute. Without this field a
	// reader cannot tell that run apart from one where no check was wired at all
	// (→226, RESULT §33.3) — the per-check outcome now rides in the
	// `completion_check` events.
	return fmt.Sprintf("contract_present=%d contract_missing=%d contract_defects=%d checks_armed=%d",
		present, missing, defects, len(h.armedCompletionChecks()))
}
