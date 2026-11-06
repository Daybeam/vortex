package chat

import (
	"strings"
	"testing"
)

// ── P1-1 guard: mode parsing and the steer text ────────────────────────
//
// The loop itself is covered by the run-level summary (`code_exec_steer`,
// `code_exec_streak_max`); what can be wrong in isolation is the switch and the
// sentence, so that is what these tests pin.

func TestParseCodeExecGuardMode(t *testing.T) {
	ok := map[string]codeExecGuardMode{
		"":      codeExecGuardSteer, // default: the defect is a hard 0.00
		"steer": codeExecGuardSteer,
		"STEER": codeExecGuardSteer,
		" log ": codeExecGuardLog,
		"count": codeExecGuardLog,
		"off":   codeExecGuardOff,
		"none":  codeExecGuardOff,
		"quiet": codeExecGuardOff,
	}
	for raw, want := range ok {
		got, parsed := parseCodeExecGuardMode(raw)
		if !parsed || got != want {
			t.Errorf("parseCodeExecGuardMode(%q) = (%v,%v), want (%v,true)", raw, got, parsed, want)
		}
	}
	// A typo must be reported, not silently treated as "the default" (→107).
	if _, parsed := parseCodeExecGuardMode("styring"); parsed {
		t.Error("an unrecognised value must not parse as valid")
	}
}

func TestCodeExecSteerText(t *testing.T) {
	out := codeExecSteerText(7, []string{"customer_reply.md"})
	if !strings.Contains(out, "7 consecutive execute_code") {
		t.Errorf("the count is the evidence, so it must be stated, got:\n%s", out)
	}
	if !strings.Contains(out, "customer_reply.md") {
		t.Errorf("the steer must name a concrete target, got:\n%s", out)
	}
	if !strings.Contains(out, "write_file") {
		t.Errorf("the steer must say what to do instead, got:\n%s", out)
	}

	noList := codeExecSteerText(6, nil)
	if strings.Contains(noList, "Still missing") {
		t.Errorf("with no declared list there is nothing to name, got:\n%s", noList)
	}
	if !strings.Contains(noList, "6 consecutive") {
		t.Errorf("the count must still be reported, got:\n%s", noList)
	}
}

// Threshold sanity: the guard must not fire on the read → compute → write rhythm
// that ordinary tasks follow, and must fire well below the observed loop (32).
func TestCodeExecGuardThresholdIsAboveNormalWork(t *testing.T) {
	const observedLoop = 32
	if defaultCodeExecStreak <= 5 {
		t.Errorf("default threshold %d is inside the normal 3–5 range", defaultCodeExecStreak)
	}
	if defaultCodeExecStreak >= observedLoop/2 {
		t.Errorf("default threshold %d is too close to the observed loop %d", defaultCodeExecStreak, observedLoop)
	}
}
