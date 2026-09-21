package core

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// ─── TUI Blocklist Tests ───────────────────────────────────────────────────

func TestCheckCommand_BlockVim(t *testing.T) {
	guard := CheckCommand("vim", []string{"file.go"})
	if !guard.Block {
		t.Fatal("vim should be blocked")
	}
}

func TestCheckCommand_BlockNano(t *testing.T) {
	guard := CheckCommand("nano", nil)
	if !guard.Block {
		t.Fatal("nano should be blocked")
	}
}

func TestCheckCommand_BlockTop(t *testing.T) {
	guard := CheckCommand("top", nil)
	if !guard.Block {
		t.Fatal("top should be blocked")
	}
}

func TestCheckCommand_BlockHtop(t *testing.T) {
	guard := CheckCommand("htop", nil)
	if !guard.Block {
		t.Fatal("htop should be blocked")
	}
}

func TestCheckCommand_BlockBtop(t *testing.T) {
	guard := CheckCommand("btop", nil)
	if !guard.Block {
		t.Fatal("btop should be blocked")
	}
}

func TestCheckCommand_BlockLess(t *testing.T) {
	guard := CheckCommand("less", []string{"file.txt"})
	if !guard.Block {
		t.Fatal("less should be blocked")
	}
}

func TestCheckCommand_BlockMore(t *testing.T) {
	guard := CheckCommand("more", []string{"file.txt"})
	if !guard.Block {
		t.Fatal("more should be blocked")
	}
}

func TestCheckCommand_BlockNm(t *testing.T) {
	guard := CheckCommand("nvim", nil)
	if !guard.Block {
		t.Fatal("nvim should be blocked")
	}
}

func TestCheckCommand_BlockEmacs(t *testing.T) {
	guard := CheckCommand("emacs", nil)
	if !guard.Block {
		t.Fatal("emacs should be blocked")
	}
}

func TestCheckCommand_AllowNormalCommands(t *testing.T) {
	for _, cmd := range []string{"ls", "cat", "grep", "git", "go", "bash", "python3", "node", "curl"} {
		guard := CheckCommand(cmd, nil)
		if guard.Block {
			t.Errorf("%s should not be blocked", cmd)
		}
	}
}

// ─── Git Auto-Rewrite Tests ────────────────────────────────────────────────

func TestCheckCommand_GitLogAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"log", "-n", "10"})
	if guard.Block {
		t.Fatal("git log should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Fatal("git log should be rewritten")
	}
	found := false
	for _, arg := range guard.RewrittenArgs {
		if arg == "--no-pager" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected --no-pager in rewritten args")
	}
}

func TestCheckCommand_GitLogAlreadyHasNoPager(t *testing.T) {
	guard := CheckCommand("git", []string{"--no-pager", "log", "-n", "10"})
	if guard.Block {
		t.Fatal("git --no-pager log should not be blocked")
	}
	// Should NOT be rewritten (already has --no-pager)
	if guard.RewrittenCommand != "" {
		t.Error("git log with --no-pager already should not be rewritten")
	}
}

func TestCheckCommand_GitStatusNoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"status"})
	if guard.Block {
		t.Fatal("git status should not be blocked")
	}
	if guard.RewrittenCommand != "" {
		t.Error("git status should not be rewritten (no pager needed)")
	}
}

func TestCheckCommand_GitDiffAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"diff"})
	if guard.Block {
		t.Fatal("git diff should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git diff should be rewritten with --no-pager")
	}
}

func TestCheckCommand_GitShowAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"show", "abc123"})
	if guard.Block {
		t.Fatal("git show should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git show should be rewritten with --no-pager")
	}
}

func TestCheckCommand_GitBlameAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"blame", "file.go"})
	if guard.Block {
		t.Fatal("git blame should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git blame should be rewritten with --no-pager")
	}
}

func TestCheckCommand_GitGrepAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"grep", "pattern", "--", "file.go"})
	if guard.Block {
		t.Fatal("git grep should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git grep should be rewritten with --no-pager")
	}
}

func TestCheckCommand_GitReflogAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"reflog"})
	if guard.Block {
		t.Fatal("git reflog should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git reflog should be rewritten with --no-pager")
	}
}

func TestCheckCommand_GitCherryAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"cherry", "upstream...branch"})
	if guard.Block {
		t.Fatal("git cherry should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git cherry should be rewritten with --no-pager")
	}
}

func TestCheckCommand_GitShortlogAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"shortlog"})
	if guard.Block {
		t.Fatal("git shortlog should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git shortlog should be rewritten with --no-pager")
	}
}

// ─── Pager Env Tests ──────────────────────────────────────────────────────

func TestPagerEnv(t *testing.T) {
	env := PagerEnv()
	if len(env) == 0 {
		t.Fatal("PagerEnv should return non-empty slice")
	}

	envMap := map[string]string{}
	for _, kv := range env {
		parts := splitEnvVar(kv)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	expected := map[string]string{
		"GIT_PAGER":     "cat",
		"PAGER":         "cat",
		"SYSTEMD_PAGER": "cat",
		"MANPAGER":      "cat",
		"LESS":          "-F",
	}
	for key, val := range expected {
		if envMap[key] != val {
			t.Errorf("PagerEnv: %s = %q, want %q", key, envMap[key], val)
		}
	}
}

func splitEnvVar(kv string) []string {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return []string{kv[:i], kv[i+1:]}
		}
	}
	return []string{kv}
}

// ─── Integration: Executor-level guard ────────────────────────────────────

func TestExecutor_BlocksTUI(t *testing.T) {
	e := NewControlledExecutor()
	ctx := context.Background()

	// Try to run vim through the executor — should be blocked
	res, err := e.Run(ctx, "vim", []string{"test.go"}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("expected status=error for blocked TUI, got %s", res.Status)
	}
	if res.Error == nil {
		t.Error("expected Error to be set for blocked command")
	}
}

func TestExecutor_BlocksTop(t *testing.T) {
	e := NewControlledExecutor()
	ctx := context.Background()

	res, err := e.Run(ctx, "top", nil, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("expected status=error for blocked top, got %s", res.Status)
	}
}

func TestExecutor_GitLogNoPager(t *testing.T) {
	// Hermetic: run against a throwaway repo with one commit so this test does
	// not depend on the outer repo's history. `git log` in a zero-commit
	// checkout exits 128, which would make the test state-dependent (and fail
	// in a fresh clone before the first commit).
	repo := t.TempDir()
	runGit := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("-c", "user.email=a@b.c", "-c", "user.name=test", "commit", "-q", "--allow-empty", "-m", "init")

	e := NewControlledExecutor()
	e.TotalTimeout = 5 * time.Second
	ctx := context.Background()

	// Run git log in a repo — should auto-inject --no-pager
	res, err := e.Run(ctx, "git", []string{"log", "--oneline", "-n", "5"}, repo, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("expected status=ok for git log, got %s (error: %v)", res.Status, res.Error)
	}
	if len(res.Stdout) == 0 {
		t.Error("git log should produce output")
	}
}

// TestCheckCommand_WindowsExeExtension tests that .exe suffix is stripped
func TestCheckCommand_WindowsExeExtension(t *testing.T) {
	guard := CheckCommand("git.exe", []string{"log"})
	if guard.Block {
		t.Fatal("git.exe should not be blocked")
	}
	if guard.RewrittenCommand == "" {
		t.Error("git.exe log should be rewritten with --no-pager")
	}
}
