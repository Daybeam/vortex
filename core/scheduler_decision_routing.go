package core

// DecisionVerdict controls short-circuit flow in handleOutput's orchestration.
type DecisionVerdict int

const (
	VerdictContinue DecisionVerdict = iota // proceed to next phase
	VerdictReturn                          // short-circuit: return from handleOutput
)

// scheduler_decision_routing.go — Phase 2: Fallback and ASAE routing.
// Will hold extracted inline blocks from handleOutput (Step 3 of the plan).
