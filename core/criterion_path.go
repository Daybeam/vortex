package core

import "path/filepath"

// criterion_path.go — the one place that decides what a relative path inside an
// exit criterion means.

// taskRelativePath resolves p against the task's output directory, which is the
// base the delegation envelope advertises as
// path_semantics "relative to task_output_dir"
// (core/spawner_delegation_workspace.go:16,19). Absolute paths pass through.
//
// O57: `file:` criteria used to be statted on the raw string, i.e. against the
// engine process cwd — a third base next to deterministic checks' outputBase
// (core/scheduler_decision_audit.go:283). A host that wrote its artifact where
// the envelope told it to therefore always failed its own exit gate, so the
// "execute inside the engine's workspace" tier of O54 recommended something it
// could not honor. Aligning deterministic checks to this same base is the open
// half of O57.
func (s *DirectedEngine) taskRelativePath(taskID, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.outputBase, taskID, p)
}
