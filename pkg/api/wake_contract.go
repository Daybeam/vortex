package api

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/daybeam/vortex/pkg/chat"
)

// Which completion contract a wake turn runs under.
//
// The gate has three states rather than the usual on/off because the thing it
// turns on has two separable halves, and the pre-registration (§6.3d) needs the
// halves to be measurable apart:
//
//	unset / off   nothing is stored; the turn is byte-identical to today
//	observe       the declaration reaches contractObservation() only
//	on / full     the declaration additionally arms the completion block
//
// Why this is one variable and not two: "act on it" without "know it" is not a
// state anyone wants, and two variables would let a batch run in it. The two
// switches the pre-registration asks for are observable here as two values.
//
// Default off, like every other behavioural change in this package. The switch
// that proves it took is not this variable but the run's own summary line:
// contract_declared on a follow-up turn is 0 on every image before this and the
// task's own count after it.
const wakeContractEnv = "VORTEX_WAKE_CONTRACT"

// wakeContractMode reads VORTEX_WAKE_CONTRACT.
//
// Unknown values are OFF, not full: a typo in a batch script must cost a run's
// measurement, never silently turn instrumentation into treatment.
func wakeContractMode() chat.WakeContractMode {
	raw := strings.TrimSpace(os.Getenv(wakeContractEnv))
	mode := chat.WakeContractOff
	switch strings.ToLower(raw) {
	case "observe":
		mode = chat.WakeContractObserve
	case "on", "1", "true", "yes", "full":
		mode = chat.WakeContractFull
	}
	logWakeContractOnce(raw, mode)
	return mode
}

// logWakeContractOnce puts the resolved mode on disk, in the same shape as
// logCompletionChecksOnce (pkg/chat/chat_completion_check.go) and for the same
// reason (→107): a value that is never printed is indistinguishable from one
// that was parsed and defaulted.
//
// This was added after the v94 smoke ran with VORTEX_WAKE_CONTRACT=observe and
// the string did not appear anywhere in the trace. Nothing was broken — the
// only consumer is a field on the run summary — but "the switch is on and this
// turn never woke" and "the switch never reached the container" then produce the
// same evidence, and telling them apart costs a rebuild.
func logWakeContractOnce(raw string, mode chat.WakeContractMode) {
	wakeContractLogged.Do(func() {
		source := "unset"
		if _, present := os.LookupEnv(wakeContractEnv); present {
			source = "env"
		}
		fmt.Fprintf(os.Stderr, "[config] %s=%q (source=%s) -> wake contract mode=%s\n",
			wakeContractEnv, raw, source, mode)
	})
}

var wakeContractLogged sync.Once
