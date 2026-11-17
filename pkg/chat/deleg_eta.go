package chat

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Expected time to completion for a delegated task.
//
// Why this file exists
// --------------------
// A wake is the only way a result reaches the model when nobody is driving the
// session, and there is no way for the model to know *when* to expect one. The
// delegate receipt says "you will be resumed"; orchestrator_task_status says
// "progress: 2/5, elapsed: 1m12s". Neither says "so this should take about
// another 1m48s", which is the only thing that would let a caller distinguish
// "still working" from "stuck" without polling on a fixed schedule.
//
// The two inputs are already in hand — created_at and the step statuses — so an
// estimate is a division, not a new data source. That matters because the
// alternative (a declared budget per task) does not exist and cannot be
// retrofitted onto 274 benchmark tasks without changing every task definition.
//
// What this is NOT
// ----------------
// It is not a deadline and it does not stop anything. Nothing in this package
// may treat the estimate as authoritative: the same number reaches the model as
// advisory text (gated, see delegETAMode) and reaches the watchdog as a *first
// check time*, not as a kill time. A task that overruns its estimate is
// evidence about the estimate, not about the task.
const (
	// stepPrior is the per-step prior used before any step has finished. 45s is
	// the median step duration of the 2026-11 harness-bench archive; it is a
	// seed, not a claim, and it is replaced by observed numbers the moment one
	// step completes.
	stepPrior = 45 * time.Second

	// etaFloor keeps a nearly-finished task from advertising "0s remaining" and
	// then not finishing, which reads as a stall to anything watching it.
	etaFloor = 15 * time.Second

	// etaMaxRemaining caps the advertised number. A task with 40 steps and one
	// done on a slow run extrapolates into hours, and a number the caller
	// dismisses as absurd is worse than no number at all.
	etaMaxRemaining = 30 * time.Minute
)

// ETABasis says where the number came from, because the three cases are not
// equally trustworthy and the text must not present them as if they were.
type ETABasis string

const (
	// ETABasisDone — the task is terminal; nothing remains.
	ETABasisDone ETABasis = "done"
	// ETABasisObserved — extrapolated from at least one finished step. This is
	// the only basis worth comparing against a wall clock.
	ETABasisObserved ETABasis = "observed"
	// ETABasisPlanned — no step has finished, so the number is stepPrior ×
	// steps. It is a guess and must be labelled as one.
	ETABasisPlanned ETABasis = "planned"
	// ETABasisUnknown — no plan yet (steps empty/absent). No number at all.
	ETABasisUnknown ETABasis = "unknown"
)

// ETA is the estimate plus the evidence for it.
type ETA struct {
	Remaining time.Duration
	Basis     ETABasis
	Done      int
	Total     int
	Elapsed   time.Duration
}

// Known reports whether Remaining means anything. Callers must check this
// rather than comparing Remaining to zero: an unknown estimate and a finished
// task both have a zero remaining time and only one of them is good news.
func (e ETA) Known() bool {
	return e.Basis != ETABasisUnknown
}

// EstimateETA computes the remaining time for a task's status dict.
//
// `st` is what DelegateStatus returns: created_at (RFC3339) and steps (map of
// step id → {status}). Both are already produced for formatTaskStatus; this
// function adds no dependency on the engine and can be tested with a literal
// map, which is the only way to pin the arithmetic before it is trusted.
func EstimateETA(st map[string]any, now time.Time) ETA {
	var out ETA
	if st == nil {
		out.Basis = ETABasisUnknown
		return out
	}

	if s, _ := st["status"].(string); isTerminalChatStatus(s) {
		out.Basis = ETABasisDone
		return out
	}

	steps, _ := st["steps"].(map[string]any)
	out.Total = len(steps)
	if out.Total == 0 {
		// No plan yet. A number here would be invented, and an invented number
		// is what makes a caller stop looking.
		out.Basis = ETABasisUnknown
		return out
	}

	for _, sv := range steps {
		sm, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		switch s, _ := sm["status"].(string); s {
		case "ok", "partial", "skipped", "failed":
			// `failed` counts as *settled*, not as progress: a step that will not
			// be retried no longer consumes time. Counting it as unfinished makes
			// a task with one dead step advertise a remainder that can never
			// shrink.
			out.Done++
		}
	}

	if created, _ := st["created_at"].(string); created != "" {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			out.Elapsed = now.Sub(t)
		}
	}

	if out.Done >= out.Total {
		// Every step settled but the task is not terminal: the remaining work is
		// the finalize step, which has no duration of its own to extrapolate.
		out.Basis = ETABasisPlanned
		out.Remaining = etaFloor
		return out
	}

	if out.Done > 0 && out.Elapsed > 0 {
		// Mean time per settled step × steps still open. Deliberately not a
		// median, because there are at most a handful of steps and a median over
		// five samples is a mean with extra steps.
		per := out.Elapsed / time.Duration(out.Done)
		out.Remaining = per * time.Duration(out.Total-out.Done)
		out.Basis = ETABasisObserved
	} else {
		out.Remaining = stepPrior * time.Duration(out.Total-out.Done)
		out.Basis = ETABasisPlanned
	}

	if out.Remaining < etaFloor {
		out.Remaining = etaFloor
	}
	if out.Remaining > etaMaxRemaining {
		out.Remaining = etaMaxRemaining
	}
	return out
}

// WatchdogDelay converts an estimate into "when is it worth looking again".
//
// 1.5× plus a flat 20s, floored at 60s. The multiplier absorbs the normal
// step-to-step spread (the mean is the estimate; half again covers a slow
// step); the floor keeps a 15s estimate from producing a check that arrives
// before the engine has written a single status row, which would be a check
// that can only ever say "no news".
//
// Returns 0 for an unknown estimate: a caller that does not know how long the
// task should take has no basis for deciding it is late, and inventing one here
// would hide that. The watchdog treats 0 as "use the blind floor".
func (e ETA) WatchdogDelay() time.Duration {
	if !e.Known() {
		return 0
	}
	if e.Basis == ETABasisDone {
		return 0
	}
	d := e.Remaining + e.Remaining/2 + 20*time.Second
	if d < 60*time.Second {
		d = 60 * time.Second
	}
	if d > etaMaxRemaining+5*time.Minute {
		d = etaMaxRemaining + 5*time.Minute
	}
	return d
}

// delegETAMode reports whether the estimate is shown to the model.
//
// Default off, for the same reason the poll lever is default off: this text
// changes what the model sees on every status call and on every delegation
// receipt, so switching it on changes the arm. It has to be measured as its own
// arm against the same image, not folded into a release — otherwise a score
// delta has two candidate causes and no way to separate them (D14).
//
// VORTEX_DELEG_ETA=on|1|true enables; anything else (including unset) is off.
func delegETAMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORTEX_DELEG_ETA"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// formatETALine renders the estimate for the model.
//
// The basis is printed, not implied. "about 1m48s (2/5 steps)" and "about 3m45s
// (no step finished yet)" are different claims, and a caller that treats the
// second as a schedule will conclude the task is stuck at the first overrun.
func formatETALine(e ETA) string {
	if !e.Known() {
		return "eta: unknown (no plan yet — do not infer progress from elapsed time alone)\n"
	}
	if e.Basis == ETABasisDone {
		return "eta: finished\n"
	}
	var b strings.Builder
	b.WriteString("eta: about " + formatDuration(e.Remaining))
	switch e.Basis {
	case ETABasisObserved:
		fmt.Fprintf(&b, " (%d/%d steps settled; extrapolated from elapsed %s)",
			e.Done, e.Total, formatDuration(e.Elapsed))
	case ETABasisPlanned:
		// Either nothing has settled yet, or everything has and only the
		// finalize step is left. Both land on the same prior, so print the real
		// count rather than the 0 the prior usually implies.
		fmt.Fprintf(&b, " (%d/%d steps settled; planning prior, not a measurement)", e.Done, e.Total)
	}
	b.WriteString("\n")
	return b.String()
}
