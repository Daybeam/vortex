package chat

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Consecutive no-output code execution guard (P1-1).
//
// execute_code is deliberately excluded from the repeated-write guard: its writes
// live inside a `code` argument that writeTargetOf cannot attribute to a path
// (see chat_write_guard.go:39-47, which explains why counting it there would
// fire during normal work). The consequence is that a run can call execute_code
// any number of times without a single guard noticing — `codeExecCalls` was
// counted and printed but never consumed by any branch.
//
// Measured on harness-bench 011 b3d-v71-r5 (2026-11-03): the delegated agent's
// deliverable landed in /workspace/workspace/, the parent could not find it, and
// it then ran 32 consecutive execute_code calls rebuilding the answer itself
// until the 40-request budget ended the run at 0.00. Nothing in the transcript
// flagged the loop; the only evidence was the exit reason.
//
// The guard counts execute_code calls since the last successful write and, on
// reaching the threshold, says once (latched per run) that no file has changed —
// with the declared-but-missing artifact names, so the model has a concrete
// target instead of "try harder".
const (
	// CodeExecGuardEnv selects the action: off | log | steer. Default steer:
	// the failure this exists for is a hard 0.00, and the threshold (6) sits an
	// order of magnitude below the observed loop (32). `log` counts and emits
	// the event without touching the result, which is the arm to run first when
	// calibrating the threshold against `code_exec_streak_max`.
	CodeExecGuardEnv = "VORTEX_CODE_EXEC_GUARD"
	// CodeExecStreakEnv overrides the threshold. Values <= 0 disable the guard.
	CodeExecStreakEnv = "VORTEX_CODE_EXEC_STREAK"

	// defaultCodeExecStreak: normal work does read → compute → write, so 3–5
	// consecutive code calls are ordinary; 6 catches the pathological loop
	// while staying above the observed normal range.
	defaultCodeExecStreak = 6
)

type codeExecGuardMode int

const (
	codeExecGuardOff codeExecGuardMode = iota
	codeExecGuardLog
	codeExecGuardSteer
)

func (m codeExecGuardMode) String() string {
	switch m {
	case codeExecGuardOff:
		return "off"
	case codeExecGuardLog:
		return "log"
	default:
		return "steer"
	}
}

// parseCodeExecGuardMode maps a raw env value to a mode. ok is false when the
// value is present but unrecognised: a typo must not silently select the arm
// that stays quiet (→107).
func parseCodeExecGuardMode(raw string) (codeExecGuardMode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "steer":
		return codeExecGuardSteer, true
	case "off", "none", "quiet":
		return codeExecGuardOff, true
	case "log", "count":
		return codeExecGuardLog, true
	default:
		return codeExecGuardSteer, false
	}
}

// codeExecGuardSettings resolves both knobs once per run and logs the values in
// force, so a batch can be stratified by what actually ran rather than by what
// the caller meant to set (→107, →146).
func codeExecGuardSettings() (codeExecGuardMode, int) {
	mode, ok := parseCodeExecGuardMode(os.Getenv(CodeExecGuardEnv))
	if !ok {
		log.Printf("[chat-harness] %s=%q is not a known mode; using %s",
			CodeExecGuardEnv, os.Getenv(CodeExecGuardEnv), mode)
	}
	streak := defaultCodeExecStreak
	if raw := strings.TrimSpace(os.Getenv(CodeExecStreakEnv)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			log.Printf("[chat-harness] %s=%q is not an integer; using %d", CodeExecStreakEnv, raw, streak)
		} else if n <= 0 {
			mode = codeExecGuardOff
		} else {
			streak = n
		}
	}
	if mode != codeExecGuardOff {
		log.Printf("[chat-harness] code_exec_guard=%s streak=%d", mode, streak)
	}
	return mode, streak
}

// codeExecSteerText names the count and the missing artifacts. It never claims
// the work is wrong — a code-only run can be legitimate — and it never claims
// execute_code changed nothing, because those writes are invisible to the guard
// by construction. The only honest statement is the one it makes: the tool that
// does write named files has not been used since.
func codeExecSteerText(streak int, missing []string) string {
	msg := fmt.Sprintf("[NO NEW FILES] %d consecutive execute_code calls, and no file has been written "+
		"with write_file since.", streak)
	if len(missing) > 0 {
		msg += " Still missing from the workspace: " + strings.Join(missing, ", ") + "."
	}
	msg += " Do not run the same computation again: either write the result straight to those paths with write_file, or state what you have already verified and what is blocking you."
	return msg
}
