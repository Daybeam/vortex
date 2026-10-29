package core

import (
	"testing"
)

func TestParseHeartbeat_None(t *testing.T) {
	d := parseHeartbeat("I will now call the read_file tool.")
	if d.Action != HeartbeatNone {
		t.Errorf("expected HeartbeatNone, got %d", d.Action)
	}
}

func TestParseHeartbeat_EmptyString(t *testing.T) {
	d := parseHeartbeat("")
	if d.Action != HeartbeatNone {
		t.Errorf("expected HeartbeatNone, got %d", d.Action)
	}
}

func TestParseHeartbeat_Complete(t *testing.T) {
	text := "CI passed. All tests green.\n[[Heartbeat::Complete]]"
	d := parseHeartbeat(text)
	if d.Action != HeartbeatComplete {
		t.Fatalf("expected HeartbeatComplete, got %d", d.Action)
	}
	if d.Report != "CI passed. All tests green." {
		t.Errorf("expected clean report, got %q", d.Report)
	}
}

func TestParseHeartbeat_Fail(t *testing.T) {
	text := "Deployment failed: container crashed.\n[[Heartbeat::Fail]]"
	d := parseHeartbeat(text)
	if d.Action != HeartbeatFail {
		t.Fatalf("expected HeartbeatFail, got %d", d.Action)
	}
	if d.Report != "Deployment failed: container crashed." {
		t.Errorf("expected clean report, got %q", d.Report)
	}
}

func TestParseHeartbeat_Sleep(t *testing.T) {
	text := "Waiting for CI...\n[[Heartbeat::Sleep::30]]"
	d := parseHeartbeat(text)
	if d.Action != HeartbeatSleep {
		t.Fatalf("expected HeartbeatSleep, got %d", d.Action)
	}
	if d.SleepSeconds != 30 {
		t.Errorf("expected 30 seconds, got %d", d.SleepSeconds)
	}
}

func TestParseHeartbeat_SleepWithWakePrompt(t *testing.T) {
	text := `Waiting for build...
[[Heartbeat::Sleep::60]]
[[Heartbeat::WakePrompt]]Check if the build at step_3 has completed. If yes, proceed to deploy.[[/Heartbeat::WakePrompt]]`
	d := parseHeartbeat(text)
	if d.Action != HeartbeatSleep {
		t.Fatalf("expected HeartbeatSleep, got %d", d.Action)
	}
	if d.SleepSeconds != 60 {
		t.Errorf("expected 60 seconds, got %d", d.SleepSeconds)
	}
	expected := "Check if the build at step_3 has completed. If yes, proceed to deploy."
	if d.WakePrompt != expected {
		t.Errorf("expected wake prompt %q, got %q", expected, d.WakePrompt)
	}
}

func TestParseHeartbeat_CompletePriorityOverSleep(t *testing.T) {
	text := "Done.\n[[Heartbeat::Sleep::30]]\n[[Heartbeat::Complete]]"
	d := parseHeartbeat(text)
	if d.Action != HeartbeatComplete {
		t.Fatalf("expected HeartbeatComplete (priority), got %d", d.Action)
	}
}

func TestParseHeartbeat_SleepZeroIgnored(t *testing.T) {
	text := "[[Heartbeat::Sleep::0]]"
	d := parseHeartbeat(text)
	if d.Action != HeartbeatNone {
		t.Errorf("expected HeartbeatNone for Sleep::0, got %d", d.Action)
	}
}

func TestParseHeartbeat_MalformedNoMatch(t *testing.T) {
	cases := []string{
		"[[Heartbeat::]]",
		"[[Heartbeat::Sleep::]]",
		"[[Heartbeat::Sleep:abc]]",
		"[[Heartbeat",
		"Heartbeat::Complete",
	}
	for _, text := range cases {
		d := parseHeartbeat(text)
		if d.Action != HeartbeatNone {
			t.Errorf("expected HeartbeatNone for %q, got %d", text, d.Action)
		}
	}
}

func TestStripHeartbeatDirectives(t *testing.T) {
	text := `Report: all good.
[[Heartbeat::Complete]]
[[Heartbeat::Sleep::10]]
[[Heartbeat::WakePrompt]]ignored[[/Heartbeat::WakePrompt]]`
	clean := stripHeartbeatDirectives(text)
	if clean != "Report: all good." {
		t.Errorf("expected 'Report: all good.', got %q", clean)
	}
}
