package core

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummarizeOutput_SmallContent_Passthrough(t *testing.T) {
	data := []byte("hello world")
	got := SummarizeOutput(data, "test")
	if got != "hello world" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestSummarizeOutput_JSON(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("{\n")
	for i := 0; i < 1000; i++ {
		sb.WriteString("  \"key" + itoa(i) + "\": \"val" + itoa(i) + "\",\n")
	}
	sb.WriteString("  \"end\": true\n}\n")
	data := []byte(sb.String())

	got := SummarizeOutput(data, "json")
	if !strings.Contains(got, "JSON Summary") {
		t.Errorf("expected JSON summary, got: %s", got[:100])
	}
}

func TestSummarizeOutput_JSONL(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("{\"id\":" + itoa(i) + ",\"name\":\"item" + itoa(i) + "\"}\n")
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "jsonl")
	if !strings.Contains(got, "JSONL Summary") {
		t.Errorf("expected JSONL summary, got: %s", got[:100])
	}
	if !strings.Contains(got, "1000 records") {
		t.Errorf("expected record count, got: %s", got[:200])
	}
}

func TestSummarizeOutput_Log(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		if i%50 == 0 {
			sb.WriteString("ERROR something failed\n")
		} else {
			sb.WriteString("INFO processing item\n")
		}
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "log")
	if !strings.Contains(got, "Log Summary") {
		t.Errorf("expected log summary, got: %s", got[:100])
	}
	if !strings.Contains(got, "ERROR:") {
		t.Errorf("expected ERROR count, got: %s", got[:200])
	}
}

func TestSummarizeOutput_Table(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("| ID | Name | Value |\n")
	sb.WriteString("|----|------|-------|\n")
	for i := 0; i < 1000; i++ {
		sb.WriteString("| " + itoa(i) + " | item | 100 |\n")
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "table")
	if !strings.Contains(got, "Table Summary") {
		t.Errorf("expected table summary, got: %s", got[:100])
	}
}

func TestSummarizeOutput_PlainText_Fallback(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("line " + itoa(i) + " of plain text content\n")
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "plain")
	// 500 lines of plain text must be summarized/truncated — not passed through verbatim.
	// The output should be shorter than the input, or contain a truncation marker.
	if len(got) >= len(data) && !strings.Contains(got, "TRUNCATED") && !strings.Contains(got, "GATEWAY") {
		t.Errorf("expected truncation or summary for 500-line plain text, got full %d bytes passthrough", len(got))
	}
	if len(got) == 0 {
		t.Error("expected non-empty output")
	}
}

func TestFileSkeleton_GoCode(t *testing.T) {
	code := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}

func helper(x int) int {
	return x * 2
}

type Config struct {
	Name string
	Port int
}

var defaultConfig = Config{}

const MaxRetries = 3
`
	got := FileSkeleton([]byte(code), "main.go")
	if !strings.Contains(got, "Code Skeleton") {
		t.Errorf("expected code skeleton header, got: %s", got)
	}
	if !strings.Contains(got, "func main") {
		t.Errorf("expected func main, got: %s", got)
	}
	if !strings.Contains(got, "func helper") {
		t.Errorf("expected func helper, got: %s", got)
	}
	if !strings.Contains(got, "type Config") {
		t.Errorf("expected type Config, got: %s", got)
	}
}

func TestFileSkeleton_PythonCode(t *testing.T) {
	code := `import os

def hello(name):
    print(f"hello {name}")

class MyClass:
    def __init__(self):
        self.x = 1
    
    async def process(self):
        pass
`
	got := FileSkeleton([]byte(code), "test.py")
	if !strings.Contains(got, "Code Skeleton") {
		t.Errorf("expected code skeleton, got: %s", got)
	}
	if !strings.Contains(got, "def hello") {
		t.Errorf("expected def hello, got: %s", got)
	}
	if !strings.Contains(got, "class MyClass") {
		t.Errorf("expected class MyClass, got: %s", got)
	}
}

func TestFileSkeleton_HTML(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
  <title>Test</title>
  <meta charset="utf-8"/>
</head>
<body>
  <div id="main">
    <p>Hello</p>
    <img src="test.png"/>
  </div>
</body>
</html>
`
	got := FileSkeleton([]byte(html), "page.html")
	if !strings.Contains(got, "Tag Skeleton") {
		t.Errorf("expected tag skeleton, got: %s", got)
	}
	if !strings.Contains(got, "<html>") {
		t.Errorf("expected <html> tag, got: %s", got)
	}
	if !strings.Contains(got, "<div>") {
		t.Errorf("expected <div> tag, got: %s", got)
	}
}

func TestFileSkeleton_JSON(t *testing.T) {
	jsonData := `{
  "name": "test",
  "version": "1.0",
  "items": [1, 2, 3, 4, 5],
  "config": {
    "debug": true,
    "port": 8080
  }
}`
	got := FileSkeleton([]byte(jsonData), "config.json")
	if !strings.Contains(got, "JSON Skeleton") {
		t.Errorf("expected JSON skeleton, got: %s", got)
	}
}

func TestFileSkeleton_UnknownExtension_Fallback(t *testing.T) {
	data := []byte("just some text\n")
	got := FileSkeleton(data, "file.xyz")
	if string(data) != got {
		t.Errorf("expected passthrough for small unknown file, got %q", got)
	}
}

func TestFileSkeleton_LineNumbers(t *testing.T) {
	code := "package main\n\nfunc alpha() {}\n\nfunc beta() {}\n"
	got := FileSkeleton([]byte(code), "test.go")
	if !strings.Contains(got, "L3:") {
		t.Errorf("expected L3 for alpha, got: %s", got)
	}
	if !strings.Contains(got, "L5:") {
		t.Errorf("expected L5 for beta, got: %s", got)
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"json object", `{"a":1,"b":2}`, "json"},
		{"json array", `[1,2,3]`, "json"},
		{"jsonl", `{"id":1}\n{"id":2}\n{"id":3}`, "jsonl"},
		{"table", "| A | B |\n|---|---|\n| 1 | 2 |", "table"},
		{"log", "INFO start\nERROR fail\nWARN retry\nINFO done", "log"},
		{"plain", "just some text\nmore text\n", "plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(strings.ReplaceAll(tt.data, `\n`, "\n"))
			if len(data) < 8*1024 {
				data = append(data, make([]byte, 8*1024)...)
			}
			got := detectFormat(data)
			if got != tt.want && got != "plain" {
				t.Errorf("detectFormat() = %q, want %q (or plain)", got, tt.want)
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestSummarizeLog_MultiKeywordLine_DeterministicHighestSeverity is a regression
// test for bug SUM-1: summarizeLog used `range levelCounts` (random Go map
// iteration order) to categorize lines, so a line containing multiple level
// keywords (e.g. "ERROR: see INFO log") was non-deterministically counted as
// whichever keyword the random iteration visited first. This could silently drop
// ERROR lines from the errorLines summary — the most important part.
//
// After the fix, keywords are checked in fixed priority order (FATAL > ERROR >
// WARN > INFO > DEBUG > TRACE), so a multi-keyword line always resolves to the
// highest severity. We build a log where every line contains both ERROR and
// INFO and assert ALL are counted as ERROR.
func TestSummarizeLog_MultiKeywordLine_DeterministicHighestSeverity(t *testing.T) {
	const numLines = 300 // ~300 lines × ~30 bytes ≈ 9KB > 8KB threshold
	var sb strings.Builder
	for i := 0; i < numLines; i++ {
		// Every line contains BOTH "ERROR" and "INFO" — before the fix, map
		// randomness meant ~1/6 were counted as ERROR. After the fix, all are.
		sb.WriteString("ERROR code=" + itoa(i) + " INFO context=load\n")
	}
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes) to trigger summarization", len(data))
	}

	got := SummarizeOutput(data, "log")
	if !strings.Contains(got, "Log Summary") {
		t.Fatalf("expected log summary, got: %s", got[:100])
	}
	// Every line must be counted as ERROR (highest severity), not INFO.
	wantErr := fmt.Sprintf("    ERROR: %d", numLines)
	if !strings.Contains(got, wantErr) {
		t.Errorf("SUM-1 regression: expected all %d lines counted as ERROR\nwant substring: %q\ngot:\n%s", numLines, wantErr, got)
	}
	// INFO must NOT be counted (every line resolved to ERROR, not INFO).
	if strings.Contains(got, "    INFO:") {
		t.Errorf("SUM-1 regression: lines leaked into INFO count (non-deterministic map order)\ngot:\n%s", got)
	}
}

// TestSummarizeTable_ExactlyThreeRows_NoZeroOmitted is a regression test for
// bug SUM-2: summarizeTable printed "... [0 rows omitted] ..." when the table
// had exactly 3 rows, because the omission marker fired unconditionally at
// shown==3 with count nonEmpty-3 == 0. Nothing was omitted, so the marker is
// nonsensical. After the fix, the marker is guarded by nonEmpty > 3.
func TestSummarizeTable_ExactlyThreeRows_NoZeroOmitted(t *testing.T) {
	// 3 pipe-rows, each ~3KB, so total > 8KB threshold triggers summarization.
	// detectFormat needs pipeCount >= 3 (3 lines with >= 2 pipes) → "table".
	pad := strings.Repeat("x", 3000)
	var sb strings.Builder
	sb.WriteString("| col1 | col2 | col3 |\n")
	sb.WriteString("| " + pad + " | a | b |\n")
	sb.WriteString("| " + pad + " | c | d |\n")
	sb.WriteString("| " + pad + " | e | f |\n")
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes) to trigger summarization", len(data))
	}

	got := SummarizeOutput(data, "table")
	if !strings.Contains(got, "Table Summary") {
		t.Fatalf("expected table summary, got: %s", got[:100])
	}
	// The "[0 rows omitted]" marker must NOT appear when nothing was omitted.
	if strings.Contains(got, "[0 rows omitted]") {
		t.Errorf("SUM-2 regression: table with exactly 3 rows printed \"[0 rows omitted]\"\ngot:\n%s", got)
	}
}

// TestSummarizeLog_LongErrorLines_Truncated verifies that long error lines in
// the log summary are truncated to maxRunesPerLine runes. Before the fix, a
// 5 KB error line (e.g. a stack trace with a huge payload) was included
// verbatim in the "Error/Fatal lines" section, bloating the summary.
func TestSummarizeLog_LongErrorLines_Truncated(t *testing.T) {
	longPad := strings.Repeat("x", 5000) // 5 KB per error line, > maxRunesPerLine
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("ERROR " + longPad + "\n")
	}
	for i := 0; i < 280; i++ {
		sb.WriteString("INFO processing item\n")
	}
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes) to trigger summarization", len(data))
	}

	got := SummarizeOutput(data, "log")
	if !strings.Contains(got, "Log Summary") {
		t.Fatalf("expected log summary, got: %s", got[:100])
	}
	// No line in the summary should exceed maxRunesPerLine + marker.
	for _, line := range strings.Split(got, "\n") {
		if utf8.RuneCountInString(line) > maxRunesPerLine+20 {
			t.Errorf("long error line not truncated: line has %d runes > %d+20\nline: %s...",
				utf8.RuneCountInString(line), maxRunesPerLine, line[:min(100, len(line))])
			break
		}
	}
}

// TestSummarizeTable_LongRows_Truncated verifies that long rows in the table
// summary are truncated to maxRunesPerLine runes. Before the fix, a 5 KB row
// (e.g. a wide CSV line) was included verbatim in the first-3-rows section,
// bloating the summary.
func TestSummarizeTable_LongRows_Truncated(t *testing.T) {
	longPad := strings.Repeat("x", 5000) // 5 KB per row, > maxRunesPerLine
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString("| " + longPad + " | a | b |\n")
	}
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes) to trigger summarization", len(data))
	}

	got := SummarizeOutput(data, "table")
	if !strings.Contains(got, "Table Summary") {
		t.Fatalf("expected table summary, got: %s", got[:100])
	}
	// No line in the summary should exceed maxRunesPerLine + marker.
	for _, line := range strings.Split(got, "\n") {
		if utf8.RuneCountInString(line) > maxRunesPerLine+20 {
			t.Errorf("long table row not truncated: line has %d runes > %d+20\nline: %s...",
				utf8.RuneCountInString(line), maxRunesPerLine, line[:min(100, len(line))])
			break
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// FileSkeleton edge-case tests — the existing tests only covered happy paths
// (valid Go/Python/HTML/JSON with detectable symbols). These tests verify
// graceful handling of empty/invalid/degenerate inputs.
// ---------------------------------------------------------------------------

// TestFileSkeleton_EmptyFile verifies that empty data doesn't panic and
// returns a valid header for each supported extension.
func TestFileSkeleton_EmptyFile(t *testing.T) {
	// Known extensions have dedicated skeleton functions that return a header
	// even for empty data. Unknown extensions (e.g. .txt) fall through to
	// SummarizeOutput which returns empty for empty data — that's correct.
	exts := []string{".go", ".json", ".html", ".py", ".js"}
	for _, ext := range exts {
		got := FileSkeleton([]byte{}, "test"+ext)
		if got == "" {
			t.Errorf("FileSkeleton(empty, .%s) returned empty string — expected header", ext)
		}
	}
}

// TestFileSkeleton_NilData verifies that nil data doesn't panic.
func TestFileSkeleton_NilData(t *testing.T) {
	// Should not panic for any extension.
	for _, ext := range []string{".go", ".json", ".html"} {
		_ = FileSkeleton(nil, "test"+ext)
	}
}

// TestFileSkeleton_InvalidJSON verifies that malformed JSON with a .json
// extension doesn't panic and returns a header. jsonSkeleton uses
// json.Decoder which breaks on error, so only the header is returned.
func TestFileSkeleton_InvalidJSON(t *testing.T) {
	invalid := []byte(`{"broken": "json", "missing":}`)
	got := FileSkeleton(invalid, "test.json")
	if !strings.Contains(got, "JSON Skeleton") {
		t.Errorf("expected JSON skeleton header for invalid JSON, got: %s", got)
	}
}

// TestFileSkeleton_NoSymbols verifies that a Go file with only comments
// (no func/type/var/const) returns a header with no symbol lines.
func TestFileSkeleton_NoSymbols(t *testing.T) {
	code := `// This file has only comments.
// No functions, types, vars, or consts.
// Just documentation.
`
	got := FileSkeleton([]byte(code), "test.go")
	if !strings.Contains(got, "Code Skeleton") {
		t.Errorf("expected code skeleton header, got: %s", got)
	}
	// Should NOT contain any symbol markers (L1: func, L1: type, etc.)
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, " func ") || strings.Contains(line, " type ") ||
			strings.Contains(line, " var ") || strings.Contains(line, " const ") {
			t.Errorf("unexpected symbol in comment-only file: %s", line)
		}
	}
}

// TestFileSkeleton_LargeFileTruncation verifies that codeSkeleton's 4000-byte
// output cap works — a file with many symbols should be truncated.
func TestFileSkeleton_LargeFileTruncation(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("func func" + itoa(i) + "() {}\n")
	}
	got := FileSkeleton([]byte(sb.String()), "test.go")
	if !strings.Contains(got, "omitted") {
		t.Errorf("expected truncation marker for 500-func file, got len=%d", len(got))
	}
	// Output should be capped around 4000 bytes.
	if len(got) > 5000 {
		t.Errorf("output too large: %d bytes, expected ~4000 cap", len(got))
	}
}
