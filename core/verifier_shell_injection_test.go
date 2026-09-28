package core

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// TestCommandPassVerifier_RejectsShellMetacharacters is a regression test for
// audit S-C1: CommandPassVerifier must reject shell metacharacters to prevent
// command injection via task graph exit criteria.
//
// Before the fix, any command string was passed directly to sh -c / cmd /c,
// allowing injection via e.g. "true; rm -rf /".
// After the fix, metacharacters ; | & $ ` > < \n \r are rejected.
//
// Reproduction: a command containing any metacharacter should return
// passed=false with a message referencing "audit S-C1".
func TestCommandPassVerifier_RejectsShellMetacharacters(t *testing.T) {
	v := &CommandPassVerifier{}

	metachars := []string{";", "|", "&", "$", "`", ">", "<", "\n", "\r"}

	for _, ch := range metachars {
		cmd := "echo hello" + ch + "rm -rf /"
		passed, msg, err := v.Verify(context.Background(), t.TempDir(), map[string]any{"command": cmd})
		if err != nil {
			t.Errorf("metachar %q: unexpected error: %v", ch, err)
		}
		if passed {
			t.Errorf("metachar %q: expected command to be rejected, but it passed", ch)
		}
		if !strings.Contains(msg, "S-C1") {
			t.Errorf("metachar %q: expected rejection message to reference S-C1, got %q", ch, msg)
		}
	}
}

// TestCommandPassVerifier_CleanCommandStillPasses verifies that the S-C1 fix
// does not over-reject legitimate commands without metacharacters.
func TestCommandPassVerifier_CleanCommandStillPasses(t *testing.T) {
	v := &CommandPassVerifier{}

	var cleanCmds []string
	if runtime.GOOS == "windows" {
		cleanCmds = []string{"echo hello", "echo test"}
	} else {
		cleanCmds = []string{"echo hello", "true", "ls"}
	}

	for _, cmd := range cleanCmds {
		passed, _, err := v.Verify(context.Background(), t.TempDir(), map[string]any{"command": cmd})
		if err != nil {
			t.Errorf("clean command %q: unexpected error: %v", cmd, err)
		}
		if !passed {
			t.Errorf("clean command %q: should still pass after S-C1 fix", cmd)
		}
	}
}

// TestCommandPassVerifier_RejectsPathSeparators is a regression test for
// audit NEW-1: the executable allowlist must reject commands containing path
// separators. Before the fix, filepath.Base stripped the path prefix for the
// allowlist check, but exec.CommandContext used the full path — allowing
// /tmp/evil/git to pass the "git" allowlist while executing the attacker's binary.
func TestCommandPassVerifier_RejectsPathSeparators(t *testing.T) {
	v := &CommandPassVerifier{}

	maliciousCmds := []string{
		"/tmp/evil/git status",
		"/usr/local/bin/git",
		"./evil.sh",
		"../evil/git",
	}

	for _, cmd := range maliciousCmds {
		passed, msg, err := v.Verify(context.Background(), t.TempDir(), map[string]any{"command": cmd})
		if err != nil {
			t.Errorf("cmd %q: unexpected error: %v", cmd, err)
		}
		if passed {
			t.Errorf("cmd %q: expected rejection for path separator, but it passed", cmd)
		}
		if !strings.Contains(msg, "NEW-1") {
			t.Errorf("cmd %q: expected message to reference NEW-1, got %q", cmd, msg)
		}
	}
}
