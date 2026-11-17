package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The mechanism must be inert unless a directory is configured. Every arm that
// does not set VORTEX_COMPLETION_CHECKS has to run exactly as it did before this
// file existed — otherwise the A/B stops being an A/B.
func TestCompletionCheckDefects_InertByDefault(t *testing.T) {
	t.Setenv(completionCheckEnv, "")
	h := &ChatHarness{SessionRoot: t.TempDir()}
	h.declaredArtifacts = []string{"report.sql"}
	if got := h.completionCheckDefects(context.Background(), "task-1", nil); got != nil {
		t.Fatalf("expected no checks without a configured dir, got %v", got)
	}
}

// A configured directory with no matching declaration is also a no-op, and a
// missing workspace must not be treated as a missing artefact.
func TestCompletionCheckDefects_NoDeclarationOrNoWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.sql.check.py"), []byte("raise SystemExit(1)\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(completionCheckEnv, dir)

	h := &ChatHarness{SessionRoot: t.TempDir()}
	h.declaredArtifacts = []string{"report.sql"}
	if got := h.completionCheckDefects(context.Background(), "task-1", nil); got != nil {
		t.Fatalf("no declaration for report.sql should mean no check, got %v", got)
	}

	h2 := &ChatHarness{} // no workspace: nothing to check against
	h2.declaredArtifacts = []string{"other.sql"}
	if got := h2.completionCheckDefects(context.Background(), "task-1", nil); got != nil {
		t.Fatalf("no SessionRoot should mean no check, got %v", got)
	}
}

// A check file whose artefact never got written is the presence gate's finding,
// not this check's: reporting it here would double-count one defect as two.
func TestCompletionCheckDefects_SkipsAbsentArtifact(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.sql.check.py"), []byte("raise SystemExit(1)\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(completionCheckEnv, dir)
	h := &ChatHarness{SessionRoot: t.TempDir()}
	h.declaredArtifacts = []string{"report.sql"}
	if got := h.completionCheckDefects(context.Background(), "task-1", nil); got != nil {
		t.Fatalf("absent artifact must not be reported as a check failure, got %v", got)
	}
}

func TestLastLine(t *testing.T) {
	cases := map[string]string{
		"":                     "check failed",
		"   \n\t\n":            "check failed",
		"boom":                 "boom",
		"line one\nboom":       "boom",
		"line one\n\nboom\n\n": "boom",
	}
	for in, want := range cases {
		if got := lastLine(in); got != want {
			t.Fatalf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}
