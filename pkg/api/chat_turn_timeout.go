package api

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The chat turn budget was a hardcoded 5 minutes. That is a *turn* clock, while
// the harness container runs on a bigger one (HB_VORTEX_DEADLINE, 1140 s in
// harness-bench), and the two had no path between them: a turn was killed with
// `context deadline exceeded` while the container still had ~14 minutes of
// budget left.
//
// Measured twice on harness-bench 026, with two different causes:
//   - v58: 11 pointless rewrites of one file (D7) filled the 300 s;
//   - v61: 15 legitimate execute_code calls filled the 300 s (D9 had just made
//     the tool output readable, so the model actually worked on it).
//
// 300 s is a product default (an interactive chat turn should not hang for
// 20 minutes), not a law. Deployments whose turn is supervised by a bigger
// outer deadline — harness containers, batch runs — must be able to say so.
const (
	// EnvChatTurnTimeout overrides DefaultChatTurnTimeout. Accepts a Go
	// duration ("15m", "900s") or a plain number of seconds.
	EnvChatTurnTimeout = "VORTEX_CHAT_TURN_TIMEOUT"

	// DefaultChatTurnTimeout is one chat turn's ceiling when the env var is unset.
	DefaultChatTurnTimeout = 5 * time.Minute

	// chatTurnMin/MaxTimeout clamp the override. The floor keeps a typo from
	// producing a turn that dies before the first provider call; the ceiling
	// keeps a typo from producing an unbounded goroutine.
	chatTurnMinTimeout = 30 * time.Second
	chatTurnMaxTimeout = time.Hour
)

// legacyChatTurnTimeoutEnvs names the environment variables that the
// harness-bench entrypoint exported for this knob before 2026-11-01. The Go
// engine has never read them — the names date from the Python engine that was
// rewritten in 2026.06 — so both were silently dead.
//
// A dead name is indistinguishable from a working one: nothing errors, nothing
// logs, and the code just keeps its default. On 2026-11-01 that meant ten
// historical rounds died with `context deadline exceeded` at ~310 s inside a
// 840 s container, nine of them in a 307–320 s band, one of them a run whose
// artifacts graded oracle=1.0 and was filed as `model_error` anyway.
//
// The fix is not a renamed constant; it is making the resolution visible. The
// resolved value is logged once per process, so every trace states which clock
// the turn actually ran on, and setting a legacy name now produces a warning
// instead of silence.
var legacyChatTurnTimeoutEnvs = []string{
	"AWEVAL_CHAT_RUN_TIMEOUT_SEC",
	"AWEVAL_PROVIDER_HTTP_TIMEOUT_SEC",
}

var chatTurnTimeoutLogOnce sync.Once

// legacyChatTurnTimeoutEnvsSet returns the legacy names that are actually set,
// taking getenv as a parameter so the check is testable without touching the
// process environment or consuming the once-only log above.
func legacyChatTurnTimeoutEnvsSet(getenv func(string) string) []string {
	var set []string
	for _, legacy := range legacyChatTurnTimeoutEnvs {
		if strings.TrimSpace(getenv(legacy)) != "" {
			set = append(set, legacy)
		}
	}
	return set
}

// logChatTurnTimeout records the resolved turn budget exactly once per process.
// Once, not per turn: the value is process-global, and a per-turn line would
// bury the one fact worth reading.
func logChatTurnTimeout(d time.Duration, source string) {
	chatTurnTimeoutLogOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[chat-turn-timeout] resolved=%s source=%s\n", d, source)
		for _, legacy := range legacyChatTurnTimeoutEnvsSet(os.Getenv) {
			fmt.Fprintf(os.Stderr,
				"[chat-turn-timeout] WARN: %s is set but this engine does not read it; "+
					"set %s instead. The turn is running on %s, not on the value in %s.\n",
				legacy, EnvChatTurnTimeout, d, legacy)
		}
	})
}

// resolveChatTurnTimeout is the pure half of chatTurnTimeout: given the raw
// env value it says which budget applies and where that decision came from.
// Keeping it pure is what lets the tests cover every branch — including the
// invalid one — without relying on a log line that only fires once.
func resolveChatTurnTimeout(raw string) (timeout time.Duration, source string, invalid bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultChatTurnTimeout, "default (env unset)", false
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return clampChatTurnTimeout(d), EnvChatTurnTimeout + "=" + raw, false
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return clampChatTurnTimeout(time.Duration(n) * time.Second), EnvChatTurnTimeout + "=" + raw, false
	}
	return DefaultChatTurnTimeout, "default (invalid " + EnvChatTurnTimeout + ")", true
}

// chatTurnTimeout resolves the turn budget: env override if valid, else the
// default. An unparseable value falls back to the default rather than failing
// the request — a bad env var must not take chat down.
func chatTurnTimeout() time.Duration {
	raw := os.Getenv(EnvChatTurnTimeout)
	d, source, invalid := resolveChatTurnTimeout(raw)
	if invalid {
		fmt.Fprintf(os.Stderr, "[chat-handlers] WARN: ignoring invalid %s=%q (want e.g. \"15m\" or 900); using %s\n",
			EnvChatTurnTimeout, strings.TrimSpace(raw), DefaultChatTurnTimeout)
	}
	logChatTurnTimeout(d, source)
	return d
}

func clampChatTurnTimeout(d time.Duration) time.Duration {
	if d < chatTurnMinTimeout {
		return chatTurnMinTimeout
	}
	if d > chatTurnMaxTimeout {
		return chatTurnMaxTimeout
	}
	return d
}
