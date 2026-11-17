package chat

import (
	"strings"
	"testing"
	"time"
)

// now is fixed so the arithmetic is the only thing under test.
var etaNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func statusAt(elapsed time.Duration, taskStatus string, steps map[string]any) map[string]any {
	return map[string]any{
		"status":     taskStatus,
		"created_at": etaNow.Add(-elapsed).Format(time.RFC3339),
		"steps":      steps,
	}
}

func stepsOf(statuses ...string) map[string]any {
	out := make(map[string]any, len(statuses))
	for i, s := range statuses {
		out["s"+string(rune('1'+i))] = map[string]any{"status": s}
	}
	return out
}

func TestEstimateETA_NilAndEmptyAreUnknown(t *testing.T) {
	for name, st := range map[string]map[string]any{
		"nil":       nil,
		"no steps":  {"status": "running"},
		"empty map": {"status": "running", "steps": map[string]any{}},
	} {
		got := EstimateETA(st, etaNow)
		if got.Basis != ETABasisUnknown || got.Known() {
			t.Errorf("%s: basis=%q known=%v, want unknown/false", name, got.Basis, got.Known())
		}
		if got.WatchdogDelay() != 0 {
			t.Errorf("%s: WatchdogDelay=%v, want 0 for an unknown estimate", name, got.WatchdogDelay())
		}
	}
}

func TestEstimateETA_TerminalIsDone(t *testing.T) {
	for _, s := range []string{"completed", "completed_with_skips", "failed", "cancelled"} {
		got := EstimateETA(statusAt(time.Hour, s, stepsOf("ok", "running")), etaNow)
		if got.Basis != ETABasisDone {
			t.Errorf("status=%s: basis=%q, want done", s, got.Basis)
		}
		if got.Remaining != 0 || got.WatchdogDelay() != 0 {
			t.Errorf("status=%s: remaining=%v delay=%v, want 0/0", s, got.Remaining, got.WatchdogDelay())
		}
	}
}

// The case that matters most: nothing settled yet. The number is a prior, and
// the basis has to say so or the caller reads it as a schedule.
func TestEstimateETA_PlannedUsesPrior(t *testing.T) {
	got := EstimateETA(statusAt(10*time.Second, "running", stepsOf("running", "pending", "pending", "pending")), etaNow)
	if got.Basis != ETABasisPlanned {
		t.Fatalf("basis=%q, want planned", got.Basis)
	}
	if want := 4 * stepPrior; got.Remaining != want {
		t.Errorf("remaining=%v, want %v", got.Remaining, want)
	}
	if got.Done != 0 || got.Total != 4 {
		t.Errorf("done/total=%d/%d, want 0/4", got.Done, got.Total)
	}
}

func TestEstimateETA_ObservedExtrapolates(t *testing.T) {
	// 2 of 5 settled after 100s ⇒ 50s per step ⇒ 3 open steps ⇒ 150s.
	got := EstimateETA(statusAt(100*time.Second, "running", stepsOf("ok", "ok", "running", "pending", "pending")), etaNow)
	if got.Basis != ETABasisObserved {
		t.Fatalf("basis=%q, want observed", got.Basis)
	}
	if want := 150 * time.Second; got.Remaining != want {
		t.Errorf("remaining=%v, want %v", got.Remaining, want)
	}
}

// A failed step will not be retried here, so it must stop inflating the
// remainder — otherwise a task with one dead step advertises a number that
// never decreases, which is the signature of a stall.
func TestEstimateETA_FailedStepCountsAsSettled(t *testing.T) {
	got := EstimateETA(statusAt(100*time.Second, "running", stepsOf("ok", "failed", "running", "pending")), etaNow)
	if got.Done != 2 {
		t.Errorf("done=%d, want 2 (ok + failed)", got.Done)
	}
	if want := 100 * time.Second; got.Remaining != want {
		t.Errorf("remaining=%v, want %v", got.Remaining, want)
	}
}

func TestEstimateETA_AllSettledButRunningFallsBackToFloor(t *testing.T) {
	got := EstimateETA(statusAt(10*time.Minute, "running", stepsOf("ok", "ok")), etaNow)
	if got.Basis != ETABasisPlanned {
		t.Fatalf("basis=%q, want planned (finalize step has no duration to extrapolate)", got.Basis)
	}
	if got.Remaining != etaFloor {
		t.Errorf("remaining=%v, want the floor %v", got.Remaining, etaFloor)
	}
}

func TestEstimateETA_CapsAndFloors(t *testing.T) {
	// Floor: 1 of 2 settled after 4s ⇒ 4s per step ⇒ 4s left, below the floor.
	got := EstimateETA(statusAt(4*time.Second, "running", stepsOf("ok", "pending")), etaNow)
	if got.Remaining != etaFloor {
		t.Errorf("floor: remaining=%v, want %v", got.Remaining, etaFloor)
	}
	// Cap: 1 of 40 settled after an hour ⇒ 39h extrapolated, capped.
	many := make([]string, 40)
	many[0] = "ok"
	for i := 1; i < 40; i++ {
		many[i] = "pending"
	}
	got = EstimateETA(statusAt(time.Hour, "running", stepsOf(many...)), etaNow)
	if got.Remaining != etaMaxRemaining {
		t.Errorf("cap: remaining=%v, want %v", got.Remaining, etaMaxRemaining)
	}
}

func TestETA_WatchdogDelayNeverChecksTooEarly(t *testing.T) {
	// The smallest possible estimate must still yield a check that is at least
	// a minute away: a check that arrives before the engine has written one
	// status row can only ever report "no news".
	small := ETA{Remaining: etaFloor, Basis: ETABasisObserved}
	if d := small.WatchdogDelay(); d < 60*time.Second {
		t.Errorf("delay=%v, want >= 60s", d)
	}
	obs := ETA{Remaining: 150 * time.Second, Basis: ETABasisObserved}
	if want := 150*time.Second + 75*time.Second + 20*time.Second; obs.WatchdogDelay() != want {
		t.Errorf("delay=%v, want %v", obs.WatchdogDelay(), want)
	}
}

func TestFormatETALine_LabelsTheBasis(t *testing.T) {
	planned := formatETALine(ETA{Remaining: 3 * time.Minute, Basis: ETABasisPlanned, Total: 4})
	if !strings.Contains(planned, "planning prior") || !strings.Contains(planned, "not a measurement") {
		t.Errorf("planned basis is not labelled as a guess: %q", planned)
	}
	obs := formatETALine(ETA{Remaining: 150 * time.Second, Basis: ETABasisObserved, Done: 2, Total: 5, Elapsed: 100 * time.Second})
	if !strings.Contains(obs, "extrapolated") || !strings.Contains(obs, "2/5") {
		t.Errorf("observed basis does not carry its evidence: %q", obs)
	}
	unk := formatETALine(ETA{Basis: ETABasisUnknown})
	if !strings.Contains(unk, "unknown") {
		t.Errorf("unknown basis must say so: %q", unk)
	}
}

// Iron law 220 applied to the gate: the flag has to be read, not assumed. An
// unset variable and a set-but-unrecognised one both mean off, and "off" has to
// be the zero value so a deployment that never sets it is unchanged.
func TestDelegETAMode_DefaultsOff(t *testing.T) {
	t.Setenv("VORTEX_DELEG_ETA", "")
	if delegETAMode() {
		t.Fatal("unset VORTEX_DELEG_ETA must mean off")
	}
	for _, v := range []string{"0", "off", "false", "no", "maybe"} {
		t.Setenv("VORTEX_DELEG_ETA", v)
		if delegETAMode() {
			t.Errorf("VORTEX_DELEG_ETA=%q must mean off", v)
		}
	}
	for _, v := range []string{"on", "1", "true", " ON "} {
		t.Setenv("VORTEX_DELEG_ETA", v)
		if !delegETAMode() {
			t.Errorf("VORTEX_DELEG_ETA=%q must mean on", v)
		}
	}
}

// The receipt is where the rule is actually delivered: the model reads it once,
// right after delegating, and cannot ask follow-up questions. So it must state
// the expectation AND what to do when it is exceeded — in one place, with a
// single allowed check, or it turns into the polling this lever exists to stop.
func TestDelegateReceipt_StatesExpectedTimeAndTheOneCheck(t *testing.T) {
	st := statusAt(30*time.Second, "running", stepsOf("running", "pending", "pending"))
	h := &ChatHarness{DelegateStatus: func(string, time.Duration) (map[string]any, bool) { return st, true }}

	t.Setenv("VORTEX_DELEG_ETA", "off")
	off := h.delegateReceipt("task-1")
	if strings.Contains(off, "expected:") {
		t.Fatalf("gate off must not add the estimate line:/n%s", off)
	}

	t.Setenv("VORTEX_DELEG_ETA", "on")
	on := h.delegateReceipt("task-1")
	if !strings.Contains(on, "expected: about") {
		t.Fatalf("gate on must state the expectation:/n%s", on)
	}
	if !strings.Contains(on, "check once with orchestrator_task_status") {
		t.Fatalf("gate on must say what to do when the estimate is exceeded:/n%s", on)
	}
	if !strings.Contains(on, "then wait for the wake") {
		t.Fatalf("the notice must not read as permission to loop:/n%s", on)
	}
}

// A finished task has no remainder to advertise; printing "expected: about 15s"
// for a task that already completed is the kind of stale number that teaches a
// caller to distrust the line entirely.
func TestDelegateReceipt_NoEstimateForFinishedTask(t *testing.T) {
	st := statusAt(time.Minute, "completed", stepsOf("ok", "ok"))
	h := &ChatHarness{DelegateStatus: func(string, time.Duration) (map[string]any, bool) { return st, true }}
	t.Setenv("VORTEX_DELEG_ETA", "on")
	if got := h.delegateReceipt("task-1"); strings.Contains(got, "expected:") {
		t.Fatalf("a finished task must not advertise an estimate:/n%s", got)
	}
}
