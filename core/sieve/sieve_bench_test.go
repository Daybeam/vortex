package sieve

import (
	"strings"
	"testing"
)

func BenchmarkSieve_CompressContext_NoCode(b *testing.B) {
	s := NewSieve(10)
	data := map[string]any{
		"summary":     "The task completed successfully with no errors.",
		"status":      "ok",
		"step_count":  5,
		"output_dir":  "/workspace/task_123",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.CompressContext(data)
	}
}

func BenchmarkSieve_CompressContext_GoCode(b *testing.B) {
	s := NewSieve(10)
	var code strings.Builder
	code.WriteString("package main\n\n")
	code.WriteString("import \"fmt\"\n\n")
	code.WriteString("func main() {\n")
	for i := 0; i < 50; i++ {
		code.WriteString("    fmt.Println(\"line ")
		code.WriteString(string(rune('a' + i%26)))
		code.WriteString("\")\n")
	}
	code.WriteString("}\n")
	data := map[string]any{
		"workspace/code/main.go": code.String(),
		"summary":                "Generated a Go program",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.CompressContext(data)
	}
}

func BenchmarkSieve_CompressContext_MixedProtected(b *testing.B) {
	s := NewSieve(10)
	var code strings.Builder
	code.WriteString("package main\n\nfunc main() {\n    println(\"hello\")\n}\n")
	data := map[string]any{
		"workspace/code/main.go": code.String(),
		"config/roles.json":      `{"role":"analyst"}`,
		"summary":                "Mixed content with protected and unprotected keys",
		"prompt/system":          "You are a helpful assistant",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.CompressContext(data)
	}
}

func BenchmarkSieve_CompressContext_NestedMap(b *testing.B) {
	s := NewSieve(10)
	var code strings.Builder
	code.WriteString("package main\n\nfunc main() {\n    println(\"hello\")\n}\n")
	data := map[string]any{
		"workspace": map[string]any{
			"code": map[string]any{
				"main.go": code.String(),
			},
		},
		"summary": "Nested structure",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.CompressContext(data)
	}
}
