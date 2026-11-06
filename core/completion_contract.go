package core

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// CompletionContractEnv is the switch that selects the completion steer's
// wording. Values: off | authorize | ask. Unset = authorize (the v70 default),
// so every existing deployment keeps its behaviour.
const CompletionContractEnv = "VORTEX_COMPLETION_CONTRACT"

// CompletionContractMode controls what the engine says when every artifact
// declared in the task's "Enforced workspace change contract" section exists on
// disk.
//
// Why a mode and not a constant (2026-11-02, harness-bench 011): the authorize
// wording ("If the work is complete, reply without calling any tool") is read as
// permission to stop *working*, not only as permission to stop *looping*. In
// v70, 5 of 14 runs wrote both artifacts out of meeting_request.json inside one
// execute_code call, never opened a single calendar file, and stopped the
// moment the steer landed; those runs scored 0.350 against 0.578 for the runs
// that read their inputs, and they are also where the reported 74% token saving
// came from. The event placement is correct (it fires only after the last tool
// call, it never truncates anything) — the sentence is what licenses the stop.
//
// The three behaviours therefore have to be separable at runtime. Separating
// them by editing the constant means one image per arm, and then the arm
// difference carries a rebuild (→70: build parameters are identity).
type CompletionContractMode int

const (
	// CompletionContractAuthorize is the original (v70) behaviour: state that
	// the artifacts exist and say that replying without tools is justified.
	CompletionContractAuthorize CompletionContractMode = iota
	// CompletionContractOff emits no marker text at all. The
	// artifacts_complete event is still emitted (with mode=off) so a trace can
	// still be read for "when would it have fired"; suppressing the event too
	// would make the arm indistinguishable from "contract never satisfied".
	CompletionContractOff
	// CompletionContractAsk replaces the license with a verification question.
	// Intended for the arm that tests whether the stop was the problem or the
	// permission was.
	CompletionContractAsk
)

// String is used in logs and in the artifacts_complete event, so a batch can be
// stratified by the mode actually in force instead of by which run-root it
// happens to sit in (→144).
func (m CompletionContractMode) String() string {
	switch m {
	case CompletionContractOff:
		return "off"
	case CompletionContractAsk:
		return "ask"
	default:
		return "authorize"
	}
}

// ParseCompletionContractMode maps a raw env value to a mode. ok is false when
// the value is present but unrecognised — the caller must not treat that as
// "the user asked for the default", because a typo would then silently run the
// arm known to early-stop (→107).
func ParseCompletionContractMode(raw string) (CompletionContractMode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "authorize":
		return CompletionContractAuthorize, true
	case "off", "none", "quiet":
		return CompletionContractOff, true
	case "ask", "verify":
		return CompletionContractAsk, true
	default:
		return CompletionContractAuthorize, false
	}
}

// CompletionContractModeFromEnv resolves the mode on every call (os.Getenv is
// not hot; Run is once per task). Deliberately not cached: caching would make
// t.Setenv unusable in tests and would hide a mid-process change of the value.
func CompletionContractModeFromEnv() CompletionContractMode {
	mode, ok := ParseCompletionContractMode(os.Getenv(CompletionContractEnv))
	if !ok {
		log.Printf("[config] %s=%q is not one of off|authorize|ask — falling back to authorize",
			CompletionContractEnv, os.Getenv(CompletionContractEnv))
	}
	return mode
}

// CompletionContractMarker returns the text appended to the last tool result,
// or "" when the mode suppresses it.
func CompletionContractMarker(mode CompletionContractMode) string {
	switch mode {
	case CompletionContractOff:
		return ""
	case CompletionContractAsk:
		return MarkerAllArtifactsAsk
	default:
		return MarkerAllArtifactsPresent
	}
}

var completionContractLogged sync.Once

// LogCompletionContractOnce prints the value actually in force, once per
// process (→107: a renamed or mistyped flag is otherwise indistinguishable from
// "parsed and applied the default").
func LogCompletionContractOnce() {
	completionContractLogged.Do(func() {
		raw, present := os.LookupEnv(CompletionContractEnv)
		mode := CompletionContractModeFromEnv()
		source := "default"
		if present {
			source = "env"
		}
		log.Printf("[config] %s=%q (source=%s) -> completion contract mode=%s",
			CompletionContractEnv, raw, source, mode)
	})
}

// ArtifactDefectsMarker is the steer emitted when every declared artifact
// exists but at least one does not satisfy the field contract the prompt stated.
//
// It replaces the completion steer for that turn rather than joining it. "All
// declared artifacts exist" and "your plan is missing `recommended`" in one
// breath is the false green of →113: the model reads the first clause, stops,
// and the run is recorded as complete over an artifact the scorer will reject.
// Existence is the signal that licenses stopping, so it must not be sent while a
// declared key is absent.
//
// Capped at maxListedDefects so a wholly wrong artifact produces a steer, not a
// wall of text that buries the first fix.
func ArtifactDefectsMarker(defects []string) string {
	const maxListedDefects = 6
	if len(defects) == 0 {
		return ""
	}
	listed := defects
	extra := 0
	if len(listed) > maxListedDefects {
		extra = len(listed) - maxListedDefects
		listed = listed[:maxListedDefects]
	}
	var b strings.Builder
	b.WriteString("[COMPLETION CONTRACT] The declared artifacts exist but do not satisfy the contract in the task prompt: ")
	b.WriteString(strings.Join(listed, "; "))
	if extra > 0 {
		fmt.Fprintf(&b, "; and %d more", extra)
	}
	b.WriteString(". The task states these fields itself — fix the artifacts, then reply.")
	return b.String()
}
