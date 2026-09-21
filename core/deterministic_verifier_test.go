package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestFileExistsVerifier_FilePresent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "output.txt")
	os.WriteFile(f, []byte("hello"), 0644)

	v := &FileExistsVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{"path": "output.txt"})
	if err != nil || !passed {
		t.Errorf("expected pass, got passed=%v msg=%q err=%v", passed, msg, err)
	}
}

func TestFileExistsVerifier_FileMissing(t *testing.T) {
	v := &FileExistsVerifier{}
	passed, msg, err := v.Verify(context.Background(), t.TempDir(), map[string]any{"path": "nonexistent.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected fail for missing file")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestFileExistsVerifier_FileEmpty(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "empty.txt")
	os.WriteFile(f, []byte{}, 0644)

	v := &FileExistsVerifier{}
	passed, _, _ := v.Verify(context.Background(), dir, map[string]any{"path": "empty.txt"})
	if passed {
		t.Error("expected fail for empty file")
	}
}

func TestFileExistsVerifier_MissingPathParam(t *testing.T) {
	v := &FileExistsVerifier{}
	passed, msg, _ := v.Verify(context.Background(), "", map[string]any{})
	if passed {
		t.Error("expected fail for missing path param")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestCommandPassVerifier_Success(t *testing.T) {
	v := &CommandPassVerifier{}
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "echo hello"
	} else {
		cmd = "true"
	}
	passed, msg, err := v.Verify(context.Background(), t.TempDir(), map[string]any{"command": cmd})
	if err != nil || !passed {
		t.Errorf("expected pass, got passed=%v msg=%q err=%v", passed, msg, err)
	}
}

func TestCommandPassVerifier_Failure(t *testing.T) {
	v := &CommandPassVerifier{}
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "exit 1"
	} else {
		cmd = "false"
	}
	passed, msg, _ := v.Verify(context.Background(), t.TempDir(), map[string]any{"command": cmd})
	if passed {
		t.Error("expected fail for failing command")
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestRunDeterministicChecks_AllPass(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "result.json")
	os.WriteFile(f, []byte(`{"status":"ok"}`), 0644)

	checks := []schemas.DeterministicCheck{
		{Type: "file_exists", Params: map[string]any{"path": "result.json"}},
	}
	passed, msg, failType := runDeterministicChecks(context.Background(), dir, checks)
	if !passed {
		t.Errorf("expected all checks to pass, got msg=%q failType=%q", msg, failType)
	}
}

func TestRunDeterministicChecks_FirstFailsShortCircuits(t *testing.T) {
	dir := t.TempDir()
	checks := []schemas.DeterministicCheck{
		{Type: "file_exists", Params: map[string]any{"path": "missing.txt"}},
		{Type: "file_exists", Params: map[string]any{"path": "also_missing.txt"}},
	}
	passed, msg, failType := runDeterministicChecks(context.Background(), dir, checks)
	if passed {
		t.Error("expected fail")
	}
	if failType != "schema_violation" {
		t.Errorf("expected schema_violation, got %q", failType)
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestRunDeterministicChecks_UnknownTypeSkipped(t *testing.T) {
	checks := []schemas.DeterministicCheck{
		{Type: "unknown_verifier", Params: map[string]any{}},
	}
	passed, _, _ := runDeterministicChecks(context.Background(), t.TempDir(), checks)
	if !passed {
		t.Error("unknown verifier types should be skipped, not fail")
	}
}

func TestRunDeterministicChecks_EmptyListPasses(t *testing.T) {
	passed, _, _ := runDeterministicChecks(context.Background(), t.TempDir(), nil)
	if !passed {
		t.Error("empty check list should pass")
	}
}
