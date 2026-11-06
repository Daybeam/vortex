package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// O39 (2026-11-05): runHub is the default mode, but it was the only entrypoint
// that never called WireCapabilityRouting. The resident daemon therefore built
// no CapabilityProfileStore and exp.get_capability_profile answered "not wired"
// for the process an operator is most likely to query.
//
// This pins the call inside runHub's own body: a whole-file Contains would keep
// passing on runExec's call alone, which is exactly how the gap survived four
// other call sites.
func TestRunHubWiresCapabilityRouting(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var body string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runHub" || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Lbrace).Offset
		end := fset.Position(fn.Body.Rbrace).Offset
		body = string(src[start:end])
		break
	}
	if body == "" {
		t.Fatal("runHub not found in main.go; update this pin if the function was renamed")
	}

	for _, want := range []string{
		"WireCapabilityRouting(",
		"NewCapabilityProfileStore(s.DB)",
		"CapProfile:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runHub no longer contains %q — the default hub must wire capability routing like the other entrypoints (O39)", want)
		}
	}
	if strings.Contains(body, "CapProfile is deliberately absent") {
		t.Error("the deliberate-absence comment is back; if that is intentional, delete this pin in the same commit (O39)")
	}
}
