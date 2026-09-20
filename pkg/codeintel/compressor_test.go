package codeintel

import (
	"strings"
	"testing"
)

func TestCompressor_CompressGo(t *testing.T) {
	src := `package test
import "fmt"
// Large function
func Large() {
	fmt.Println("1")
	fmt.Println("2")
	fmt.Println("3")
}
func Small() string { return "ok" }
`
	comp := NewCompressor(5) // Low threshold for test
	compressed, ok := comp.CompressGo("test.go", src)
	if !ok {
		t.Fatal("compression failed")
	}

	if !strings.Contains(compressed, "[AST COMPRESSED]") {
		t.Error("header missing")
	}
	if strings.Contains(compressed, "Println") {
		t.Error("implementation detail leaked")
	}
	if !strings.Contains(compressed, "func Large()") {
		t.Error("signature missing")
	}
}

func TestCompressor_GetDetails(t *testing.T) {
	src := `package test
func Target() {
	// Inside target
	println("found me")
}
func Other() {}
`
	comp := NewCompressor(100)
	details, err := comp.GetDetails("test.go", src, "Target")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(details, "found me") {
		t.Error("detail missing from extracted symbol")
	}
}
