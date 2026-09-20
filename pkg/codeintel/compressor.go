package codeintel

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

// Compressor provides AST-aware code shrinking.
type Compressor struct {
	LineThreshold int // Trigger compression if file > threshold lines
}

func NewCompressor(threshold int) *Compressor {
	if threshold <= 0 {
		threshold = 100 // Default to 100 lines
	}
	return &Compressor{LineThreshold: threshold}
}

// CompressGo reduces a Go source string to its public API / signatures.
func (c *Compressor) CompressGo(filename, src string) (string, bool) {
	lines := strings.Split(src, "\n")
	if len(lines) < c.LineThreshold {
		return src, false
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return src, false // Fallback to raw if parse fails
	}

	// Transform AST: strip function bodies
	ast.Inspect(f, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok {
			if fn.Body != nil {
				// Replace body with a placeholder comment
				fn.Body.List = []ast.Stmt{
					&ast.ExprStmt{
						X: &ast.CallExpr{
							Fun: ast.NewIdent("/* ... implementation hidden; call 'orchestrator_get_code_details' with path and symbol to view ... */"),
						},
					},
				}
			}
		}
		return true
	})

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return src, false
	}

	header := fmt.Sprintf("// [AST COMPRESSED] This file is large (%d lines). implementation details are hidden to save tokens.\n", len(lines))
	return header + buf.String(), true
}

// GetDetails retrieves the full body of a specific function or the whole file.
func (c *Compressor) GetDetails(filename, src, symbol string) (string, error) {
	if symbol == "" {
		return src, nil // Return whole file
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return "", err
	}

	var foundBody string
	ast.Inspect(f, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok {
			if fn.Name.Name == symbol {
				// Use printer to get the original source range
				start := fset.Position(fn.Pos()).Offset
				end := fset.Position(fn.End()).Offset
				foundBody = src[start:end]
				return false
			}
		}
		// Also check type methods
		return true
	})

	if foundBody != "" {
		return foundBody, nil
	}

	return "", fmt.Errorf("symbol %q not found in %s", symbol, filename)
}
