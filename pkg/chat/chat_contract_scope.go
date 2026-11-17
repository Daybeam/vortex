package chat

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/core"
)

// Contract scope on the chat path: the section split by column, plus the
// workspace snapshot taken before the first model turn.
//
// Why this is not part of declaredArtifacts (RESULT §28.7): on the three tasks
// whose contract is "Create: none / Modify: X" (262/263/264) the deliverable is
// already in the seed workspace, so every presence test answers "done" at t=0
// (→221). What such a run can still get wrong is what it leaves behind — 263
// delivered a working `submission/service.py` and still scored 0.0 because
// `state.db` was sitting next to it, and the scorer compares the directory to the
// seed. The engine had no code path that enumerated a directory at all.
//
// This adds exactly that path and nothing else: no new engine capability, no
// model call, and completely inert on the 271 tasks whose contract creates
// something — their completion signal is the declared-artifact one and must not
// change (268 tasks must behave byte-identically).
type contractScope struct {
	present  bool
	create   []string
	modify   []string
	baseline map[string]struct{}
	// lastExtra is set by newPathDefects and read when the steer is chosen, so
	// the marker can tell "the artifact is the wrong shape" from "the workspace
	// grew something". Empty on every task except the modify-only three.
	lastExtra []string
}

// initContract is the one place Run parses the contract section
// and returns the Create column, so that call site keeps its shape: chat
// harness.go is under a shrink-only ratchet (Q6-1) and may not grow.
func (h *ChatHarness) initContract(query string) []string {
	c := core.ParseDeclaredContract(query)
	h.contract.present, h.contract.create, h.contract.modify = c.Present, c.Create, c.Modify

	// Snapshot only when the extras check applies. A failure disables the check
	// rather than guessing: with no baseline every seed file would read as "new"
	// and the steer would accuse the model of creating the very files the task
	// handed it.
	if h.SessionRoot != "" && h.extrasArmed() {
		if snap, err := core.SnapshotWorkspace(h.SessionRoot); err == nil {
			h.contract.baseline = snap
		}
	}
	return c.Create
}

// extrasArmed reports whether this contract has the modify-only shape: a section
// exists, names nothing to create, and names something to modify. That is
// exactly 3 of the 274 tasks (262/263/264), so the blast radius is bounded by the
// task set, not by a threshold anyone can drift.
func (h *ChatHarness) extrasArmed() bool {
	return h.contract.present && len(h.contract.create) == 0 && len(h.contract.modify) > 0
}

// contractArmed reports whether Run's completion block should run at all. It runs
// for the declared-artifact case exactly as before, and additionally for the
// extras case — where it can only ever produce a defect steer, never the "all
// present" one, because Run guards that branch on len(declaredArtifacts) > 0.
//
// That guard is the whole point of →221: for a modify-only task "all declared
// artifacts exist" is true before the model starts, so emitting it would be a
// false green light on turn one and would spend the one-shot latch.
func (h *ChatHarness) contractArmed() bool {
	return len(h.declaredArtifacts) > 0 || (h.extrasArmed() && h.contract.baseline != nil)
}

// newPathDefects reports paths that appeared in the workspace during the run.
//
// One line, not one per path: the steer channel caps at six entries and lists
// them, so N lines would spend that cap on a single violation (→197: one
// violation must not be counted as N).
func (h *ChatHarness) newPathDefects() []string {
	h.contract.lastExtra = nil
	if h.SessionRoot == "" || h.contract.baseline == nil || !h.extrasArmed() {
		return nil
	}
	extra, err := core.WorkspaceNewPaths(h.SessionRoot, h.contract.baseline)
	if err != nil {
		return nil // an unreadable workspace is a harness fault, not the model's
	}
	if len(extra) == 0 {
		return nil
	}
	h.contract.lastExtra = extra
	return []string{fmt.Sprintf(
		"the workspace holds %d path(s) the contract does not allow: %s",
		len(extra), strings.Join(extra, ", "))}
}
