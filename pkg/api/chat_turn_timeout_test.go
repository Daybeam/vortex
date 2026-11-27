package api

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetChatTurnTimeoutLog lets a test observe the once-per-process resolution
// line more than once. Test-only: production code must never reset it.
func resetChatTurnTimeoutLog() { chatTurnTimeoutLogOnce = sync.Once{} }

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// TestChatTurnTimeout_DefaultAndOverride pins the D8 half that is safe to land
// without touching any measurement: the turn budget is configurable, the
// default is unchanged (5 min), and a bad value cannot take chat down.
func TestChatTurnTimeout_DefaultAndOverride(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", DefaultChatTurnTimeout},
		{"15m", 15 * time.Minute},
		{"900s", 900 * time.Second},
		{"900", 900 * time.Second},         // bare seconds
		{" 900 ", 900 * time.Second},       // tolerate whitespace
		{"1m", time.Minute},                // above the floor, honoured as-is
		{"5s", chatTurnMinTimeout},         // clamped up
		{"48h", chatTurnMaxTimeout},        // clamped down
		{"banana", DefaultChatTurnTimeout}, // unparseable → default, not a failure
		{"-30s", chatTurnMinTimeout},       // negative → clamped up
	}
	for _, c := range cases {
		t.Setenv(EnvChatTurnTimeout, c.env)
		if got := chatTurnTimeout(); got != c.want {
			t.Errorf("%s=%q → %s, want %s", EnvChatTurnTimeout, c.env, got, c.want)
		}
	}
}

// TestChatTurnTimeout_DefaultIsFiveMinutes guards the product default: an
// interactive chat turn must not silently start running for the container's
// full budget just because someone edited this file.
func TestChatTurnTimeout_DefaultIsFiveMinutes(t *testing.T) {
	t.Setenv(EnvChatTurnTimeout, "")
	if DefaultChatTurnTimeout != 5*time.Minute {
		t.Fatalf("DefaultChatTurnTimeout drifted to %s; changing it changes every deployment", DefaultChatTurnTimeout)
	}
}

// TestResolveChatTurnTimeout_EveryBranch covers the pure half directly, so the
// invalid case is asserted on its own terms instead of through a log line that
// only fires once per process.
func TestResolveChatTurnTimeout_EveryBranch(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		source  string
		invalid bool
	}{
		{"", DefaultChatTurnTimeout, "default (env unset)", false},
		{"   ", DefaultChatTurnTimeout, "default (env unset)", false},
		{"900", 900 * time.Second, EnvChatTurnTimeout + "=900", false},
		{" 900 ", 900 * time.Second, EnvChatTurnTimeout + "=900", false},
		{"15m", 15 * time.Minute, EnvChatTurnTimeout + "=15m", false},
		{"2s", chatTurnMinTimeout, EnvChatTurnTimeout + "=2s", false},
		{"48h", chatTurnMaxTimeout, EnvChatTurnTimeout + "=48h", false},
		{"banana", DefaultChatTurnTimeout, "default (invalid " + EnvChatTurnTimeout + ")", true},
	}
	for _, c := range cases {
		got, source, invalid := resolveChatTurnTimeout(c.raw)
		if got != c.want {
			t.Errorf("resolveChatTurnTimeout(%q) timeout = %s, want %s", c.raw, got, c.want)
		}
		if source != c.source {
			t.Errorf("resolveChatTurnTimeout(%q) source = %q, want %q", c.raw, source, c.source)
		}
		if invalid != c.invalid {
			t.Errorf("resolveChatTurnTimeout(%q) invalid = %v, want %v", c.raw, invalid, c.invalid)
		}
	}
}

// TestLegacyChatTurnTimeoutEnvsSet pins the detection that turns a dead knob
// into a warning. The legacy names come from the harness entrypoint's
// Python-era exports; the Go engine has never read them, so a round that sets
// only those silently runs on the default.
func TestLegacyChatTurnTimeoutEnvsSet(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"none", map[string]string{}, nil},
		{"blank does not count", map[string]string{"AWEVAL_CHAT_RUN_TIMEOUT_SEC": "  "}, nil},
		{"single", map[string]string{"AWEVAL_CHAT_RUN_TIMEOUT_SEC": "840"}, []string{"AWEVAL_CHAT_RUN_TIMEOUT_SEC"}},
		{"both", map[string]string{
			"AWEVAL_CHAT_RUN_TIMEOUT_SEC":      "840",
			"AWEVAL_PROVIDER_HTTP_TIMEOUT_SEC": "840",
		}, []string{"AWEVAL_CHAT_RUN_TIMEOUT_SEC", "AWEVAL_PROVIDER_HTTP_TIMEOUT_SEC"}},
	}
	for _, c := range cases {
		got := legacyChatTurnTimeoutEnvsSet(func(k string) string { return c.env[k] })
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestChatTurnTimeout_LogsResolvedValue records which clock the turn is on.
// This is the part that would have caught the 2026-11-01 defect: ten rounds
// died at ~310 s inside a 1800 s task because nothing in the trace said the
// turn was still on the 300 s default.
func TestChatTurnTimeout_LogsResolvedValue(t *testing.T) {
	resetChatTurnTimeoutLog()
	t.Setenv(EnvChatTurnTimeout, "840")

	out := captureStderr(t, func() { chatTurnTimeout() })
	if !strings.Contains(out, "[chat-turn-timeout] resolved=14m0s") {
		t.Fatalf("resolved line missing or wrong, got:\n%s", out)
	}
	if !strings.Contains(out, "source="+EnvChatTurnTimeout+"=840") {
		t.Fatalf("source not reported, got:\n%s", out)
	}
}

// TestChatTurnTimeout_LogsDefaultSource is the other half: an unset knob must
// say so, because "resolved=5m0s source=default (env unset)" is exactly the
// line that was missing from every trace above.
func TestChatTurnTimeout_LogsDefaultSource(t *testing.T) {
	resetChatTurnTimeoutLog()
	t.Setenv(EnvChatTurnTimeout, "")

	out := captureStderr(t, func() { chatTurnTimeout() })
	if !strings.Contains(out, "resolved=5m0s") || !strings.Contains(out, "source=default (env unset)") {
		t.Fatalf("default resolution not reported, got:\n%s", out)
	}
}

// TestChatTurnTimeout_WarnsWhenOnlyLegacyNameIsSet makes the dead-knob case
// loud: the operator set a variable, so the engine must say it is not reading
// it rather than leaving the default in place without comment.
func TestChatTurnTimeout_WarnsWhenOnlyLegacyNameIsSet(t *testing.T) {
	resetChatTurnTimeoutLog()
	t.Setenv(EnvChatTurnTimeout, "")
	t.Setenv("AWEVAL_CHAT_RUN_TIMEOUT_SEC", "840")

	out := captureStderr(t, func() { chatTurnTimeout() })
	if !strings.Contains(out, "WARN: AWEVAL_CHAT_RUN_TIMEOUT_SEC is set but this engine does not read it") {
		t.Fatalf("no warning for the dead legacy name, got:\n%s", out)
	}
	if !strings.Contains(out, EnvChatTurnTimeout) {
		t.Fatalf("warning does not name the variable to use instead, got:\n%s", out)
	}
}
