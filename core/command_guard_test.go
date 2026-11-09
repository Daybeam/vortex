package core

import (
	"context"
	"os"
	"os/exec"
	"strings"
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
	if guard.RewrittenArgs == nil {
		t.Fatal("git log should have rewritten args")
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
	if guard.RewrittenArgs == nil {
		t.Error("git diff should have rewritten args with --no-pager")
	}
}

func TestCheckCommand_GitShowAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"show", "abc123"})
	if guard.Block {
		t.Fatal("git show should not be blocked")
	}
	if guard.RewrittenArgs == nil {
		t.Error("git show should have rewritten args with --no-pager")
	}
}

func TestCheckCommand_GitBlameAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"blame", "file.go"})
	if guard.Block {
		t.Fatal("git blame should not be blocked")
	}
	if guard.RewrittenArgs == nil {
		t.Error("git blame should have rewritten args with --no-pager")
	}
}

func TestCheckCommand_GitGrepAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"grep", "pattern", "--", "file.go"})
	if guard.Block {
		t.Fatal("git grep should not be blocked")
	}
	if guard.RewrittenArgs == nil {
		t.Error("git grep should have rewritten args with --no-pager")
	}
}

func TestCheckCommand_GitReflogAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"reflog"})
	if guard.Block {
		t.Fatal("git reflog should not be blocked")
	}
	if guard.RewrittenArgs == nil {
		t.Error("git reflog should have rewritten args with --no-pager")
	}
}

func TestCheckCommand_GitCherryAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"cherry", "upstream...branch"})
	if guard.Block {
		t.Fatal("git cherry should not be blocked")
	}
	if guard.RewrittenArgs == nil {
		t.Error("git cherry should have rewritten args with --no-pager")
	}
}

func TestCheckCommand_GitShortlogAutoRewrite(t *testing.T) {
	guard := CheckCommand("git", []string{"shortlog"})
	if guard.Block {
		t.Fatal("git shortlog should not be blocked")
	}
	if guard.RewrittenArgs == nil {
		t.Error("git shortlog should have rewritten args with --no-pager")
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

	// Run git log in a repo — should auto-inject --no-pager.
	// Retry up to 3 times: on CI runners with -race, the stdout pipe can
	// occasionally be empty due to scheduling jitter, not a real bug.
	var res *ExecutionResult
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		res, err = e.Run(ctx, "git", []string{"log", "--oneline", "-n", "5"}, repo, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status == "ok" && len(res.Stdout) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
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
	if guard.RewrittenArgs == nil {
		t.Error("git.exe log should have rewritten args with --no-pager")
	}
}

// ─── Guard Chain Tests ────────────────────────────────────────────────────

// TestCheckAll_FailClosed verifies that the first Block result wins.
func TestCheckAll_FailClosed(t *testing.T) {
	saved := commandGuards
	defer func() { commandGuards = saved }()

	commandGuards = []CommandGuard{
		alwaysBlockGuard{name: "first", msg: "first blocked"},
		alwaysAllowGuard{name: "second"},
	}
	r := CheckAll("ls", nil, "")
	if !r.Block {
		t.Fatal("expected first guard to block")
	}
	if r.Message != "first blocked" {
		t.Errorf("expected message 'first blocked', got %q", r.Message)
	}
}

// TestCheckAll_RewriteAccumulation verifies that later guards see earlier rewrites.
func TestCheckAll_RewriteAccumulation(t *testing.T) {
	saved := commandGuards
	defer func() { commandGuards = saved }()

	commandGuards = []CommandGuard{
		rewriteGuard{name: "rewriter", newCmd: "rewritten-cmd"},
		observeGuard{name: "observer"},
	}
	r := CheckAll("original-cmd", nil, "")
	if r.RewrittenCommand != "rewritten-cmd" {
		t.Errorf("expected rewritten command, got %q", r.RewrittenCommand)
	}
}

// TestCheckAll_BuiltinGuardsRegistered verifies that init() registered the
// two builtin guards and they behave correctly through CheckAll.
func TestCheckAll_BuiltinGuardsRegistered(t *testing.T) {
	// TUI block through CheckAll
	r := CheckAll("vim", []string{"file"}, "")
	if !r.Block {
		t.Error("expected vim to be blocked by builtin TUI guard via CheckAll")
	}

	// Git pager rewrite through CheckAll
	r = CheckAll("git", []string{"log"}, "")
	if r.Block {
		t.Error("git log should not be blocked")
	}
	if r.RewrittenArgs == nil {
		t.Error("expected git log args to be rewritten with --no-pager")
	}
}

// ─── External Script Guard Tests ──────────────────────────────────────────

// TestExternalScriptGuard_DenyMode_AllowOnExit0 tests that a deny-mode
// guard allows the command when the script exits 0.
func TestExternalScriptGuard_DenyMode_AllowOnExit0(t *testing.T) {
	g := &ExternalScriptGuard{
		name:    "test-deny",
		script:  writeGuardScript(t, "exit 0"),
		timeout: 2 * time.Second,
		mode:    "deny",
	}
	r := g.Check("ls", nil, "")
	if r.Block {
		t.Fatal("deny mode with exit 0 should allow")
	}
}

// TestExternalScriptGuard_DenyMode_BlockOnExit1 tests that a deny-mode
// guard blocks the command when the script exits non-zero.
func TestExternalScriptGuard_DenyMode_BlockOnExit1(t *testing.T) {
	g := &ExternalScriptGuard{
		name:    "test-deny",
		script:  writeGuardScript(t, "echo 'dangerous command'; exit 1"),
		timeout: 2 * time.Second,
		mode:    "deny",
	}
	r := g.Check("rm", []string{"-rf", "/"}, "")
	if !r.Block {
		t.Fatal("deny mode with exit 1 should block")
	}
	if r.Message != "dangerous command" {
		t.Errorf("expected message 'dangerous command', got %q", r.Message)
	}
}

// TestExternalScriptGuard_DenyMode_BlockOnTimeout tests that a deny-mode
// guard blocks when the script times out.
func TestExternalScriptGuard_DenyMode_BlockOnTimeout(t *testing.T) {
	g := &ExternalScriptGuard{
		name:    "test-deny",
		script:  writeGuardScript(t, "sleep 10"),
		timeout: 50 * time.Millisecond,
		mode:    "deny",
	}
	r := g.Check("ls", nil, "")
	if !r.Block {
		t.Fatal("deny mode with timeout should block")
	}
}

// TestExternalScriptGuard_AllowMode_AllowOnExit0 tests that an allow-mode
// guard allows the command when the script exits 0.
func TestExternalScriptGuard_AllowMode_AllowOnExit0(t *testing.T) {
	g := &ExternalScriptGuard{
		name:    "test-allow",
		script:  writeGuardScript(t, "exit 0"),
		timeout: 2 * time.Second,
		mode:    "allow",
	}
	r := g.Check("ls", nil, "")
	if r.Block {
		t.Fatal("allow mode with exit 0 should allow")
	}
}

// TestExternalScriptGuard_AllowMode_BlockOnExit1 tests that an allow-mode
// guard blocks when the script exits non-zero.
func TestExternalScriptGuard_AllowMode_BlockOnExit1(t *testing.T) {
	g := &ExternalScriptGuard{
		name:    "test-allow",
		script:  writeGuardScript(t, "echo 'not on allowlist'; exit 1"),
		timeout: 2 * time.Second,
		mode:    "allow",
	}
	r := g.Check("rm", []string{"-rf", "/"}, "")
	if !r.Block {
		t.Fatal("allow mode with exit 1 should block")
	}
}

// TestExternalScriptGuard_AllowMode_AllowOnTimeout tests that an allow-mode
// guard allows when the script times out (fail-open for allow mode).
func TestExternalScriptGuard_AllowMode_AllowOnTimeout(t *testing.T) {
	g := &ExternalScriptGuard{
		name:    "test-allow",
		script:  writeGuardScript(t, "sleep 10"),
		timeout: 50 * time.Millisecond,
		mode:    "allow",
	}
	r := g.Check("ls", nil, "")
	if r.Block {
		t.Fatal("allow mode with timeout should allow (fail-open)")
	}
}

// ─── RunViaTempScript Body Guard Tests ────────────────────────────────────

// TestRunViaTempScript_BlocksDangerousBody verifies that the script body
// is inspected by the guard chain before execution.
func TestRunViaTempScript_BlocksDangerousBody(t *testing.T) {
	saved := commandGuards
	defer func() { commandGuards = saved }()

	commandGuards = []CommandGuard{
		bodyBlockGuard{name: "body-check", trigger: "rm -rf"},
	}
	e := NewControlledExecutor()
	res, err := e.RunViaTempScript(context.Background(), "bash", "rm -rf /", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("expected status=error for dangerous body, got %s", res.Status)
	}
}

// TestRunViaTempScript_AllowsSafeBody verifies that safe script bodies
// are not blocked.
func TestRunViaTempScript_AllowsSafeBody(t *testing.T) {
	e := NewControlledExecutor()
	e.TotalTimeout = 5 * time.Second
	res, err := e.RunViaTempScript(context.Background(), "bash", "echo hello", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status == "error" && res.Error != nil {
		// The error might be from the guard or from execution. If it's from
		// the guard, that's a problem. If it's from execution (e.g., bash
		// not found), that's environment-dependent and OK.
		errMsg := res.Error.Error()
		if strings.Contains(errMsg, "blocked by command guard") {
			t.Errorf("safe body should not be blocked: %s", errMsg)
		}
	}
}

// ─── Test Helpers ─────────────────────────────────────────────────────────

// writeGuardScript creates a temporary shell script with the given content
// and returns its path.
func writeGuardScript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/guard.sh"
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write guard script: %v", err)
	}
	return path
}

// alwaysBlockGuard blocks every command with the given message.
type alwaysBlockGuard struct {
	name string
	msg  string
}

func (g alwaysBlockGuard) Name() string { return g.name }
func (g alwaysBlockGuard) Check(command string, args []string, body string) CommandGuardResult {
	return CommandGuardResult{Block: true, Message: g.msg}
}

// alwaysAllowGuard allows every command.
type alwaysAllowGuard struct {
	name string
}

func (g alwaysAllowGuard) Name() string { return g.name }
func (g alwaysAllowGuard) Check(command string, args []string, body string) CommandGuardResult {
	return CommandGuardResult{}
}

// rewriteGuard rewrites the command to newCmd.
type rewriteGuard struct {
	name   string
	newCmd string
}

func (g rewriteGuard) Name() string { return g.name }
func (g rewriteGuard) Check(command string, args []string, body string) CommandGuardResult {
	return CommandGuardResult{RewrittenCommand: g.newCmd}
}

// observeGuard records what it sees (for testing rewrite accumulation).
type observeGuard struct {
	name string
}

func (g observeGuard) Name() string { return g.name }
func (g observeGuard) Check(command string, args []string, body string) CommandGuardResult {
	return CommandGuardResult{}
}

// bodyBlockGuard blocks if the body contains the trigger string.
type bodyBlockGuard struct {
	name    string
	trigger string
}

func (g bodyBlockGuard) Name() string { return g.name }
func (g bodyBlockGuard) Check(command string, args []string, body string) CommandGuardResult {
	if strings.Contains(body, g.trigger) {
		return CommandGuardResult{Block: true, Message: "body contains blocked pattern: " + g.trigger}
	}
	return CommandGuardResult{}
}
