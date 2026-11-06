package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The third door of ironlaw 3: the variable is set, the check is enabled, and
// the call site actually reaches execution. The offline corpus proves the check
// body is correct; this proves it is *wired*. It runs the real execTool path —
// HandleCoreTool -> executeCode -> the configured sandbox (local here) — with a
// check that unconditionally fails, then one that passes.
func TestCompletionCheckDefects_ExecutesDeclaredCheck(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if _, err := exec.LookPath("python"); err != nil {
			t.Skip("no python runtime on PATH; execution path cannot be exercised here")
		}
	}

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "report.sql"), []byte("create view v as select 1 as x;\n"), 0o644); err != nil {
		t.Fatalf("write artefact: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "report.sql.check.py")
	t.Setenv(completionCheckEnv, dir)

	h := &ChatHarness{SessionRoot: ws, OutputBase: t.TempDir()}
	h.declaredArtifacts = []string{"report.sql"}

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write check: %v", err)
		}
	}

	write("import sys\nsys.stderr.write('view v is broken\\n')\nsys.exit(1)\n")
	got := h.completionCheckDefects(context.Background(), "e2e")
	if len(got) != 1 || !strings.Contains(got[0], "view v is broken") {
		t.Fatalf("a failing check must surface one defect line, got %v", got)
	}
	if !strings.HasPrefix(got[0], "report.sql: ") {
		t.Fatalf("defect must name its artefact, got %q", got[0])
	}

	write("import sys\nprint('ok')\nsys.exit(0)\n")
	if got := h.completionCheckDefects(context.Background(), "e2e"); got != nil {
		t.Fatalf("a passing check must add no defect, got %v", got)
	}
}
