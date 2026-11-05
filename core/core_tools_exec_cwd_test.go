package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression test for the live incident in Telegram session
// telegram_5821547814 (2026-11-03 19:33): execute_code failed with a chdir
// error because outputs/chat_<sessionID> did not exist. Nothing on the chat
// path creates it — the only MkdirAll calls for that directory are in the DAG
// scheduler, so a chat turn always got a cwd the OS refused.
//
// Note how the pre-existing execute_code tests in
// core_tools_eval_fix_test.go:25 open with their own os.MkdirAll: the suite
// built the directory by hand and therefore never exercised the production
// path. These cases deliberately do not.

// TestExecuteCode_CreatesMissingTaskDir asserts the directory is created. It
// ignores the returned error on purpose: the interpreter may be absent from
// PATH, and either way the defect under test is the cwd, not the script.
func TestExecuteCode_CreatesMissingTaskDir(t *testing.T) {
	base := t.TempDir()
	const taskID = "chat_telegram_5821547814"

	taskDir := filepath.Join(base, taskID)
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s must not exist (err=%v)", taskDir, err)
	}

	_, _ = HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     `print("cwd probe")`,
		"language": "python",
	}, base, taskID)

	info, err := os.Stat(taskDir)
	if err != nil {
		t.Fatalf("execute_code left its cwd missing: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("execute_code cwd must be a directory, got mode %v", info.Mode())
	}
}

// TestExecuteCode_SurfacesUncreatableCwd pins that a cwd which cannot be
// created is reported as such instead of being handed to the OS to fail on.
func TestExecuteCode_SurfacesUncreatableCwd(t *testing.T) {
	base := t.TempDir()
	const taskID = "chat_blocked"

	// A regular file sitting where taskDir would go: MkdirAll cannot win.
	blocker := filepath.Join(base, taskID)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     `print("cwd probe")`,
		"language": "python",
	}, base, taskID)
	if err == nil || !strings.Contains(err.Error(), "failed to create execution directory") {
		t.Fatalf("expected a creation failure, got: %v", err)
	}
}

// TestExecCwdFor_PrefersSessionRoot pins the branch order: a harness with
// HARNESSBENCH_WORKSPACE set must keep executing in the workspace, not in the
// per-task directory (the model's relative paths are resolved against it).
func TestExecCwdFor_PrefersSessionRoot(t *testing.T) {
	base := t.TempDir()
	sessionRoot := filepath.Join(base, "workspace")
	if err := os.MkdirAll(sessionRoot, 0755); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(base, "outputs", "chat_x")
	v := NewSafePathValidator(taskDir, sessionRoot, []string{sessionRoot})

	cwd, err := execCwdFor(v, taskDir)
	if err != nil {
		t.Fatalf("execCwdFor: %v", err)
	}
	if cwd != sessionRoot {
		t.Errorf("cwd = %q, want session root %q", cwd, sessionRoot)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Errorf("taskDir must stay untouched when a session root exists: %v", err)
	}
}
