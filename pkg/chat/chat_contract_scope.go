package chat

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
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

	// wakeDeclared / wakeMode / wakeSeeded carry the same contract into a wake
	// turn, which has no prompt to parse it from (see SeedWakeContract). All
	// three stay zero on an ordinary turn, so nothing here can change one.
	wakeDeclared  []string
	wakeContracts []schemas.ArtifactContract
	wakeMode      WakeContractMode
	wakeSeeded    bool
}

// initContract is the one place Run parses the contract section
// and returns the Create column, so that call site keeps its shape: chat
// harness.go is under a shrink-only ratchet (Q6-1) and may not grow.
func (h *ChatHarness) initContract(query string) []string {
	c := core.ParseDeclaredContract(query)
	h.contract.present, h.contract.create, h.contract.modify = c.Present, c.Create, c.Modify

	// A wake turn's query is followUpTriggerFmt — a sentence, not a task prompt —
	// so c.Present is false there and there is nothing to parse. The declaration
	// comes from the finished task's outcome instead (SeedWakeContract), and only
	// when the query really carries no section of its own: a turn that states its
	// contract must never be overridden by one inherited from a task.
	if h.contract.wakeMode != WakeContractOff && !c.Present && len(h.contract.wakeDeclared) > 0 {
		h.contract.wakeSeeded = true
		return h.contract.wakeDeclared
	}

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

func (h *ChatHarness) initDeclaredFields(query string) {
	declaredFields := core.ParseDeclaredArtifactFields(query)
	h.declaredFields = declaredFields
	h.declaredContracts = nil
	if h.contract.wakeSeeded && declaredFields == nil {
		h.declaredContracts = schemas.CloneArtifactContracts(h.contract.wakeContracts)
		h.declaredFields = make(map[string][]string, len(h.declaredContracts))
		for _, contract := range h.declaredContracts {
			h.declaredFields[contract.Path] = append([]string(nil), contract.RequiredFields...)
		}
	}
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
	if !h.completionEnforced() {
		return false
	}
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

// ── The contract on a wake turn (FINDINGS §6.3d, P2) ────────────────────────
//
// A wake turn's query is followUpTriggerFmt: a sentence, not a task prompt, so
// initContract finds no "[COMPLETION CONTRACT]" section. That is not an
// occasional shape — every wake turn in the archive is like this. 142 follow-up
// turns across 20 tasks (041 accounts for 47), all 142 with
// contract_declared=0, while the same runs' front turns declare 2–3 artifacts.
// So the wake turn is the one turn that cannot say whether the workspace still
// holds what the task was asked to produce, and §6.3's four-cell table has no
// column for "the wake turn knew what it was supposed to deliver".
//
// Wiring it in is a treatment, not a gauge. Four places can change what the
// model sees or what a tool returns, enumerated here so they are not discovered
// later by diffing a transcript:
//
//  1. contractObservation() — the [chat-turn-summary] line goes from na to
//     numbers. Log-only, never reaches the model. On in every non-off mode.
//  2. contractArmed() — the one-shot "everything is present" steer and the
//     bounded defect steer, appended to the last tool result of a turn. Full.
//  3. execTool's delegation skip — delegate_to_orchestrator answers
//     MarkerDelegationSkipped instead of submitting. Full.
//  4. formatTaskStatus's terminal action — a poll answers "declared 1
//     artifact(s); present 1; missing: …" instead of "read the output_files
//     above and answer". Full.
//
// Observe is the arm that can be read as instrumentation; full is the arm that
// has to be pre-registered as treatment (§6.3d). The pre-registration's
// falsification line — "if wake turns start being steer-rewritten, split into
// two switches" — is answered by construction: the split already exists, so
// observe cannot silently become full.
type WakeContractMode int

const (
	// WakeContractOff is the default and the only mode that leaves the turn
	// byte-identical to the one before this existed: nothing is stored, so the
	// harness cannot even report a declaration it was given.
	WakeContractOff WakeContractMode = iota
	// WakeContractObserve fills in the declaration for the summary line only.
	// Every consumer of presence is held back (see completionEnforced).
	WakeContractObserve
	// WakeContractFull additionally lets the completion block act on it.
	WakeContractFull
)

// String makes the mode printable: an int that renders as %!s(...) in a log line
// is worse than no line at all, because it looks like the log itself is broken.
func (m WakeContractMode) String() string {
	switch m {
	case WakeContractOff:
		return "off"
	case WakeContractObserve:
		return "observe"
	case WakeContractFull:
		return "full"
	}
	return "unknown"
}

// SeedWakeContract tells a harness whose next Run is a wake turn what the
// finished task declared. Called on the per-turn clone, never on a shared
// harness: the declaration belongs to one turn and to one task.
//
// Empty declared is not an error and stores nothing — 011/029/030-style tasks
// whose prompt names no artifact must not acquire a contract by being woken, or
// presence would be computed against a list nobody wrote.
func (h *ChatHarness) SeedWakeContract(contracts []schemas.ArtifactContract, mode WakeContractMode) {
	if h == nil || mode == WakeContractOff || len(contracts) == 0 {
		return
	}
	h.contract.wakeMode = mode
	h.contract.wakeContracts = schemas.CloneArtifactContracts(contracts)
	h.contract.wakeDeclared = make([]string, 0, len(contracts))
	for _, contract := range contracts {
		if contract.Path != "" {
			h.contract.wakeDeclared = append(h.contract.wakeDeclared, contract.Path)
		}
	}
}

// completionEnforced reports whether presence may be acted on during this turn.
//
// False in exactly one case: the declaration was inherited from a wake seed and
// the mode is observe. Ordinary turns — no seed — are always true, so the 274
// tasks' front turns keep the behaviour they have today.
func (h *ChatHarness) completionEnforced() bool {
	return !(h.contract.wakeSeeded && h.contract.wakeMode != WakeContractFull)
}

// declaredForStatus is what an orchestrator_task_status answer may quote.
//
// Observe-only returns nil so the wake turn's poll reads exactly like the
// pre-change one. The counts are not neutral: formatTerminalAction replaces
// "read the output_files above and answer" with "declared N artifact(s); present
// M", which is a second information channel arriving before any steer — it would
// make the observe arm a treatment and the 2×2 unattributable.
func (h *ChatHarness) declaredForStatus() []string {
	if !h.completionEnforced() {
		return nil
	}
	return h.declaredArtifacts
}
