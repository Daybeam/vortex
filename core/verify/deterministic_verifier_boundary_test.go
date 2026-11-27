package verify

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// O58: deterministic checks ran from the shared outputs root and accepted
// absolute/traversing paths, letting one task grade itself with another task's
// files or read host files. These cases fail before the confinement fix.
func TestRunDeterministicChecks_ConfinesPathsToTaskDir(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "taskA")
	siblingDir := filepath.Join(root, "taskB")
	for _, dir := range []string{taskDir, siblingDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(taskDir, "report.json")
	sibling := filepath.Join(siblingDir, "report.json")
	outside := filepath.Join(root, "outside.json")
	for _, path := range []string{inside, sibling, outside} {
		if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(taskDir, "entity.go"), []byte("package task\nfunc TaskEntity() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "entity.go"), []byte("package outside\nfunc EscapedEntity() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trace := `{"trace":[{"tool_name":"read_file","arguments":{"path":"report.json"}}]}`
	if err := os.WriteFile(inside, []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		check schemas.DeterministicCheck
		pass  bool
	}{
		{"task file", schemas.DeterministicCheck{Type: "file_exists", Params: map[string]any{"path": "report.json"}}, true},
		{"absolute file", schemas.DeterministicCheck{Type: "file_exists", Params: map[string]any{"path": inside}}, false},
		{"parent escape", schemas.DeterministicCheck{Type: "file_exists", Params: map[string]any{"path": "../outside.json"}}, false},
		{"sibling task", schemas.DeterministicCheck{Type: "file_exists", Params: map[string]any{"path": "../taskB/report.json"}}, false},
		{"absolute plan", schemas.DeterministicCheck{Type: "plan_checkboxes", Params: map[string]any{"path": inside}}, false},
		{"absolute code dir", schemas.DeterministicCheck{Type: "code_entities", Params: map[string]any{"entities": []any{"TaskEntity"}, "search_dir": taskDir}}, false},
		{"parent code dir", schemas.DeterministicCheck{Type: "code_entities", Params: map[string]any{"entities": []any{"EscapedEntity"}, "search_dir": ".."}}, false},
		{"absolute trace", schemas.DeterministicCheck{Type: "read_coverage", Params: map[string]any{"required_files": []any{"report.json"}, "trace_file": inside}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			passed, msg, _ := RunDeterministicChecks(context.Background(), taskDir, []schemas.DeterministicCheck{tt.check})
			if passed != tt.pass {
				t.Fatalf("passed=%v, want %v: %s", passed, tt.pass, msg)
			}
		})
	}
}

// O58: lexical confinement alone is insufficient when an in-task symlink points
// outside the task. The verifier must resolve links before reading or walking.
func TestRunDeterministicChecks_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "taskA")
	outsideDir := filepath.Join(root, "outside")
	for _, dir := range []string{taskDir, outsideDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(taskDir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	passed, _, _ := RunDeterministicChecks(context.Background(), taskDir, []schemas.DeterministicCheck{{
		Type: "file_exists", Params: map[string]any{"path": "link.txt"},
	}})
	if passed {
		t.Fatal("symlink escape satisfied file_exists")
	}
}

// O58: command arguments were unconstrained and CombinedOutput was copied into
// steps[].last_error. Reject path escapes and retain only a sanitized category.
func TestCommandPassVerifier_RejectsEscapingArguments(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "outside.txt")
	for _, command := range []string{"cat " + absolute, "cat ../outside.txt", "cat " + filepath.Join("..", "outside.txt")} {
		passed, msg, err := (&CommandPassVerifier{}).Verify(context.Background(), t.TempDir(), map[string]any{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		if passed || !strings.Contains(msg, "O58") {
			t.Fatalf("escaping argument was not rejected: command=%q passed=%v msg=%q", command, passed, msg)
		}
	}
}

func TestCommandPassVerifier_FailureDoesNotEchoOutput(t *testing.T) {
	t.Setenv("VORTEX_COMMAND_ALLOWLIST", "go")
	const marker = "O58_OUTPUT_MARKER_9131"
	passed, msg, err := (&CommandPassVerifier{}).Verify(context.Background(), ".", map[string]any{
		"command": "go tool " + marker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if passed {
		t.Fatal("missing go tool must fail")
	}
	if strings.Contains(msg, marker) || strings.Contains(msg, "output:") {
		t.Fatalf("command output leaked into failure reason: %q", msg)
	}
	if !strings.Contains(msg, "command failed") {
		t.Fatalf("missing sanitized failure category: %q", msg)
	}
}

func TestCommandPassVerifier_TaskDirectoryIsWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix test executable")
	}
	t.Setenv("VORTEX_COMMAND_ALLOWLIST", "test")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("task"), 0o644); err != nil {
		t.Fatal(err)
	}
	passed, msg, err := (&CommandPassVerifier{}).Verify(context.Background(), dir, map[string]any{"command": "test -f marker"})
	if err != nil || !passed {
		t.Fatalf("command did not run in task dir: passed=%v msg=%q err=%v", passed, msg, err)
	}
}
