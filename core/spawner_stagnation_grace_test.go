package core

import (
	"reflect"
	"testing"
)

// replayStagnation drives the pure decision function the same way
// spawner_dospawn.go does, so the resulting sequence can be asserted against
// the historical behaviour.
func replayStagnation(grace, threshold, maxTurns int) []string {
	hinted := false
	hintAt := 0
	out := make([]string, 0, maxTurns)
	for repeats := 1; repeats <= maxTurns; repeats++ {
		act := stagnationStep(repeats, hintAt, grace, threshold, hinted)
		out = append(out, act.String())
		switch act {
		case stagHint:
			hinted = true
			hintAt = repeats
		case stagKill:
			// Real code returns from doSpawn here; stop replaying.
			return out
		}
	}
	return out
}

// TestStagnationStep_GraceZeroIsHistorical pins the A/B control arm.
// grace=0 MUST reproduce the pre-2026-10-23 behaviour exactly: hint once when
// the count first exceeds the threshold, then hard-kill on the very next turn.
// If this test ever fails, the "baseline" arm of the experiment is not a
// baseline and any measured delta is meaningless.
func TestStagnationStep_GraceZeroIsHistorical(t *testing.T) {
	got := replayStagnation(0, 3, 10)
	want := []string{"none", "none", "none", "hint", "kill"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grace=0 diverged from historical behaviour:\n got %v\nwant %v", got, want)
	}
}

// TestStagnationStep_GraceDefersKill verifies the grace window delays only the
// kill — the hint still fires on the same turn as before.
func TestStagnationStep_GraceDefersKill(t *testing.T) {
	got := replayStagnation(3, 3, 12)
	want := []string{
		"none", "none", "none", // repeats 1..3: at/below threshold
		"hint",                 // repeats 4: first turn over threshold
		"grace", "grace", "grace", // repeats 5..7: grace window
		"kill", // repeats 8: hintAt(4)+grace(3) exceeded
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grace=3 sequence wrong:\n got %v\nwant %v", got, want)
	}
}

// TestStagnationStep_HintPositionUnchanged guards the invariant that made the
// grace window measurable: the hint fires at the same turn regardless of grace.
// If the hint moved, the treatment arm would differ in two ways at once
// (intervention timing + kill timing) and attribution would be lost.
func TestStagnationStep_HintPositionUnchanged(t *testing.T) {
	for _, grace := range []int{0, 1, 3, 10} {
		seq := replayStagnation(grace, 3, 6)
		if idx := seqIndex(seq, "hint"); idx != 3 {
			t.Fatalf("grace=%d: hint fired at step %d, want 3 (seq=%v)", grace, idx, seq)
		}
	}
}

// TestStagnationStep_NoActionAtOrBelowThreshold keeps the false-positive guard:
// exactly maxConsecutiveDuplicates repeats must not trigger anything.
func TestStagnationStep_NoActionAtOrBelowThreshold(t *testing.T) {
	for repeats := 0; repeats <= 3; repeats++ {
		if act := stagnationStep(repeats, 0, 100, 3, false); act != stagNone {
			t.Fatalf("repeats=%d should be stagNone, got %v", repeats, act)
		}
	}
}

// TestStagnationStep_LargeGraceNeverKillsWithinBudget documents what a large
// grace actually buys: the step survives to max turns instead of dying early.
func TestStagnationStep_LargeGraceNeverKillsWithinBudget(t *testing.T) {
	got := replayStagnation(1000, 3, 20)
	if seqIndex(got, "kill") != -1 {
		t.Fatalf("grace=1000 should not kill within 20 turns, got %v", got)
	}
	if n := seqCount(got, "hint"); n != 1 {
		t.Fatalf("hint must be injected exactly once, got %d (%v)", n, got)
	}
}

func seqIndex(seq []string, want string) int {
	for i, s := range seq {
		if s == want {
			return i
		}
	}
	return -1
}

func seqCount(seq []string, want string) int {
	n := 0
	for _, s := range seq {
		if s == want {
			n++
		}
	}
	return n
}
