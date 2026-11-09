package chat

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// Q6-1 ratchet (decision 2026-11-04: "不拆", with one stated precondition —
// "Run 只许缩不许长，新功能一律进 helper，这条要靠 sizecheck 守住").
//
// sizecheck cannot hold that line today. scripts/size_baseline.txt froze
// chat_harness.go at 1137 and Run at 401 (its lines 9-10) while the tree already
// measures 1525 and 645, so the gate is red at HEAD before anyone edits it, and
// "red" carries no information about a fresh few lines. A baseline that lags
// reality by 244 lines is exactly the failure mode this test guards against, so
// the ceiling here moves only when the code does: growth fails, shrink fails
// until the constant is lowered.
//
// Both measurements reuse sizecheck's own conventions — newline count for the
// file (scripts/sizecheck.go:162) and End().Line - Pos().Line + 1 for the method
// (scripts/sizecheck.go:173) — so the gate and this test can never disagree
// about what "how long is Run" means.
//
// Lower these two numbers when the code shrinks. Raising them needs the user's
// decision on Q6-1 again, not a test edit.
const (
	chatHarnessFileCeiling = 1555
	chatHarnessRunCeiling  = 667
)

func TestChatHarnessSize_RatchetOnlyTightens(t *testing.T) {
	const path = "chat_harness.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	checkSize(t, "file "+path, bytes.Count(src, []byte("\n")), chatHarnessFileCeiling,
		"the file is over its frozen size limit already; new code belongs in a helper file")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	runLines := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" || !isChatHarnessReceiver(fn) {
			continue
		}
		runLines = fset.Position(fn.End()).Line - fset.Position(fn.Pos()).Line + 1
	}
	if runLines == 0 {
		t.Fatal("(*ChatHarness).Run not found — the ratchet would silently pass on a renamed method")
	}
	checkSize(t, "func (*ChatHarness).Run", runLines, chatHarnessRunCeiling,
		"Q6-1: Run may only shrink; extract the new branch into a helper instead")
}

// isChatHarnessReceiver matches the pointer receiver form sizecheck's funcName
// renders as "(*ChatHarness)".
func isChatHarnessReceiver(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "ChatHarness"
}

func checkSize(t *testing.T, what string, got, ceiling int, growHint string) {
	t.Helper()
	switch {
	case got > ceiling:
		t.Errorf("%s is %d lines, ceiling %d: %s", what, got, ceiling, growHint)
	case got < ceiling:
		t.Errorf("%s shrank to %d lines: lower this test's ceiling constant to %d — a ratchet that lags reality stops enforcing (register Q6-1)",
			what, got, got)
	}
}
