package core

// mount_gate.go — dynamic-MCP mount authorization helpers.

// effectiveSubmitterIsAdmin reports whether the caller who submitted the task
// owning this spawn is admin tier.
//
// SECURITY FIX (2026-10-21, issue R1): the mount gate previously received a
// hardcoded `true` from the execution path, so planner/replan/surgery steps
// could mount arbitrary MCPs regardless of the submitter. The submitter's tier
// now travels on the task graph (TaskGraph.SubmitterIsAdmin, recorded once at
// Submit time from StepInput.SubmitterIsAdmin, which is json:"-" so a caller
// cannot forge it). Reading it here covers every spawn — including
// engine-internal sub-spawns (planner/auditor/arbiter/replay) that build their
// own SpawnRequest without touching this field.
func effectiveSubmitterIsAdmin(hub *ContextHub) bool {
	return hub != nil && hub.Graph != nil && hub.Graph.SubmitterIsAdmin
}
