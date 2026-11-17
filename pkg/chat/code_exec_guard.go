package chat

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// Consecutive no-output exploration guard (P1-1).
//
// execute_code is deliberately excluded from the repeated-write guard: its writes
// live inside a `code` argument that writeTargetOf cannot attribute to a path
// (see chat_write_guard.go:39-47, which explains why counting it there would
// fire during normal work). The consequence is that a run can call execute_code
// any number of times without a single guard noticing — `codeExecCalls` was
// counted and printed but never consumed by any branch.
//
// search_archive joined the streak in P1: it is another read path that can
// consume the whole budget without producing a file. read_file/grep/find stay
// with checkDelegationDrift (delegation_drift.go) so one call never receives
// two competing corrections.
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
	// codeExecGuardEscalate is the opt-in hard arm: the same steer at the
	// threshold, a second level that ends the current tool batch, and a third
	// that ends the run. It is NOT the default: ending a run on a read-only
	// task is indistinguishable from a broken harness to the user, so the
	// production arm stays advisory until this arm has been measured.
	codeExecGuardEscalate
)

func (m codeExecGuardMode) String() string {
	switch m {
	case codeExecGuardOff:
		return "off"
	case codeExecGuardLog:
		return "log"
	case codeExecGuardEscalate:
		return "escalate"
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
	case "escalate", "escalation":
		return codeExecGuardEscalate, true
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
	msg := fmt.Sprintf("[NO NEW FILES] %d consecutive exploration calls (execute_code / search_archive), "+
		"and no file has been written with write_file since.", streak)
	if len(missing) > 0 {
		msg += " Still missing from the workspace: " + strings.Join(missing, ", ") + "."
	}
	msg += " Do not run the same computation again: either write the result straight to those paths with write_file, or state what you have already verified and what is blocking you."
	return msg
}

// ── Exploration guard state and escalation (P1) ──────────────────────────

// isExploreTool reports the calls that count as "exploring without producing".
//
// execute_code: its writes live inside the `code` argument, so writeTargetOf
// cannot attribute them to a path (chat_write_guard.go). search_archive: it is
// another read path whose results are never a file. read_file/grep/find are
// deliberately NOT here — checkDelegationDrift already steers on them at the
// same call count, and two different corrections on the same call is noise,
// not emphasis.
func isExploreTool(name string) bool {
	return name == "execute_code" || name == "search_archive"
}

// exploreGuard holds the per-run counters the after-call guards mutate. They
// used to be locals in Run; the extraction keeps Run inside its Q6-1 ratchet
// (harness_size_ratchet_test.go) while the escalate arm adds behaviour.
type exploreGuard struct {
	streak    int // consecutive exploration calls since the last successful write
	streakMax int
	steers    int
	steered   bool // default arm: one correction per run
	level     int  // escalate arm: highest threshold reached (0..3)

	writeTotal          int
	writeTargets        map[string]struct{}
	writeCounts         map[string]int
	repeatedWriteSteers int
}

// toolAfterRule is what the guards decided for the current call, beyond the
// text they appended. Only the escalate arm can set either field.
type toolAfterRule struct {
	stopBatch bool // level 2: skip the rest of this turn's non-write calls
	endRun    bool // level 3: end the run as an exploration-truncated partial
}

// afterToolCall applies the write-repetition and no-output guards to one
// successful model call. The default arm is the pre-P1-1 behaviour plus
// search_archive; only the opt-in escalate arm returns a stopping decision.
func (h *ChatHarness) afterToolCall(tc schemas.ToolCall, result *string, g *exploreGuard,
	mode codeExecGuardMode, limit int, declared []string, emit func(ChatEvent) error) toolAfterRule {
	if target := h.writeTargetOf(tc.Name, tc.Arguments); target != "" {
		g.writeTotal++
		g.writeTargets[target] = struct{}{}
		// Any real write breaks the no-output streak: the model is producing
		// files again, which is exactly what the guard wants it to do.
		g.streak = 0
		// Only the escalate arm re-arms its levels. The default arm's single
		// correction stays latched for the run, unchanged from P1-1.
		if mode == codeExecGuardEscalate {
			g.level = 0
		}
		if n, steer := writeGuardSteer(g.writeCounts, tc.Name, target); steer != "" {
			g.repeatedWriteSteers++
			emit(ChatEvent{Type: "repeated_write", Data: target, Meta: map[string]any{"tool": tc.Name, "count": n}})
			*result += "\n\n" + steer
		}
	}
	if mode == codeExecGuardOff || !isExploreTool(tc.Name) {
		return toolAfterRule{}
	}

	g.streak++
	if g.streak > g.streakMax {
		g.streakMax = g.streak
	}
	missing := []string(nil)
	if h.SessionRoot != "" && len(declared) > 0 {
		missing = core.MissingArtifacts(h.SessionRoot, declared)
	}

	if mode != codeExecGuardEscalate {
		if !g.steered && g.streak >= limit {
			g.steered = true
			g.steers++
			emit(ChatEvent{Type: "code_exec_steer", Meta: map[string]any{
				"streak": g.streak, "mode": mode.String(), "missing": len(missing),
			}})
			if mode == codeExecGuardSteer {
				*result += "\n\n" + codeExecSteerText(g.streak, missing)
			}
		}
		return toolAfterRule{}
	}

	// Escalate arm: limit steers, 2*limit stops the batch, 4*limit ends the run.
	switch {
	case g.level < 1 && g.streak >= limit:
		g.level, g.steers = 1, g.steers+1
		emit(ChatEvent{Type: "exploration_escalated", Meta: map[string]any{
			"level": 1, "streak": g.streak, "missing": len(missing)}})
		*result += "\n\n" + codeExecSteerText(g.streak, missing)
	case g.level < 2 && g.streak >= 2*limit:
		g.level, g.steers = 2, g.steers+1
		emit(ChatEvent{Type: "exploration_escalated", Meta: map[string]any{
			"level": 2, "streak": g.streak, "missing": len(missing)}})
		*result += "\n\n" + explorationEscalateText(g.streak, missing)
		return toolAfterRule{stopBatch: true}
	case g.level < 3 && g.streak >= 4*limit:
		g.level, g.steers = 3, g.steers+1
		emit(ChatEvent{Type: "exploration_escalated", Meta: map[string]any{
			"level": 3, "streak": g.streak}})
		*result += "\n\n" + explorationExhaustedText(g.streak)
		return toolAfterRule{endRun: true}
	}
	return toolAfterRule{}
}

// explorationEscalateText is the level-2 correction: the count, the missing
// names, and the fact that the rest of the batch will not run.
func explorationEscalateText(streak int, missing []string) string {
	msg := fmt.Sprintf("[NO NEW FILES — ESCALATION] %d consecutive exploration calls and still no file written. "+
		"The remaining tool calls in this turn are skipped. Write the deliverable with write_file now", streak)
	if len(missing) > 0 {
		msg += "; still missing: " + strings.Join(missing, ", ")
	}
	return msg + ", or answer with what you have verified and what is blocking you."
}

// explorationExhaustedText is the level-3 correction. It is attached to the last
// tool result the model will see, so it says what is about to happen rather
// than asking for another attempt.
func explorationExhaustedText(streak int) string {
	return fmt.Sprintf("[EXPLORATION CEILING] %d consecutive calls without writing a file. "+
		"This run is ending; reply with what has been verified and what is blocking you. "+
		"Anything already written stays in the workspace.", streak)
}

// batchHasWrite reports whether the rest of this turn's batch contains a call
// that names an output file. The level-2 batch stop must not swallow a write
// the model already queued behind the exploration calls: stopping there would
// turn a correction into the very failure it is trying to prevent.
func (h *ChatHarness) batchHasWrite(calls []schemas.ToolCall) bool {
	for _, tc := range calls {
		if h.writeTargetOf(tc.Name, tc.Arguments) != "" {
			return true
		}
	}
	return false
}

// skipRemainingCalls closes the transcript for calls that will not run. Every
// tool call the model issued must have exactly one tool result before the next
// request, so a skipped call gets a synthetic result rather than silence.
func skipRemainingCalls(calls []schemas.ToolCall, emit func(ChatEvent) error, messages *[]ChatMessage) {
	for _, tc := range calls {
		msg := fmt.Sprintf("[tool %s] not executed: the exploration ceiling for this turn was reached. "+
			"Write the deliverable with write_file, or answer with what you have verified.", tc.Name)
		_ = emit(ChatEvent{Type: "tool_result", Data: msg,
			Meta: map[string]any{"tool": tc.Name, "skipped": true}})
		*messages = append(*messages, ChatMessage{
			Role:    "tool",
			Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, msg),
		})
	}
}
