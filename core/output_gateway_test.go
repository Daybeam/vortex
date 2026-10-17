package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAdaptiveOutputResult_Small(t *testing.T) {
	smallData := []byte("hello world\nline 2\n")
	res := AdaptiveOutputResult(smallData, "echo test", "")
	if !bytes.Equal(res, smallData) {
		t.Fatalf("expected small data to pass through unmodified")
	}
}

func TestAdaptiveOutputResult_MediumTruncate(t *testing.T) {
	var buf bytes.Buffer
	for i := 1; i <= 300; i++ {
		buf.WriteString(string(bytes.Repeat([]byte("a"), 50)) + fmtLine(i) + "\n")
	}
	buf.WriteString("CRITICAL error: something went wrong in middle\n")
	for i := 302; i <= 400; i++ {
		buf.WriteString(string(bytes.Repeat([]byte("b"), 50)) + fmtLine(i) + "\n")
	}

	data := buf.Bytes()
	if len(data) < 8*1024 {
		data = append(bytes.Repeat([]byte("x"), 9*1024), data...)
	}

	res := AdaptiveOutputResult(data, "long_cmd", "")
	if !bytes.Contains(res, []byte("OUTPUT TRUNCATED")) && !bytes.Contains(res, []byte("OUTPUT GATEWAY")) {
		t.Fatalf("expected output to be truncated or gatewayed")
	}
}

func fmtLine(i int) string {
	return fmt.Sprintf(" line number %d", i)
}

// ---------------------------------------------------------------------------
// Extreme / edge-case tests — backported from vx-toolkit-0.1.1 (vx-extreme.py)
// ---------------------------------------------------------------------------

// TestAdaptiveOutputResult_NeverBloat_SingleHugeLine verifies that a single
// pathological line (200 KB) does not cause the output to exceed the input.
// Before the fix, AdaptiveOutputResult returned the full content plus a header
// (200 KB + ~100 bytes > 200 KB). After the fix, per-line truncation caps the
// line to maxRunesPerLine runes and the never-bloat guard ensures output < input.
//
// Backport from vx-toolkit extreme test E3 (10 MB single line).
func TestAdaptiveOutputResult_NeverBloat_SingleHugeLine(t *testing.T) {
	huge := strings.Repeat("x", 200*1024) // 200 KB single line, > 128 KB threshold
	data := []byte(huge)
	got := AdaptiveOutputResult(data, "test", "")
	if len(got) >= len(data) {
		t.Errorf("never-bloat violated: output %d bytes >= input %d bytes", len(got), len(data))
	}
}

// TestAdaptiveOutputResult_PerLineTruncation verifies that no line in the output
// exceeds maxRunesPerLine + truncation marker. Before the fix, a 500 KB line
// in the head section appeared verbatim in the output. After the fix, it is
// truncated to maxRunesPerLine runes.
//
// Backport from vx-toolkit extreme test E3 (10 MB single line in head).
func TestAdaptiveOutputResult_PerLineTruncation(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(strings.Repeat("y", 500*1024) + "\n") // 500 KB first line → in head
	for i := 0; i < 300; i++ {
		sb.WriteString("line " + itoa(i) + "\n")
	}
	data := []byte(sb.String())
	got := string(AdaptiveOutputResult(data, "test", ""))

	maxLineRunes := 0
	for _, line := range strings.Split(got, "\n") {
		r := utf8.RuneCountInString(line)
		if r > maxLineRunes {
			maxLineRunes = r
		}
	}
	// maxRunesPerLine + len("…[truncated]") = 1000 + 13 = 1013. Allow small margin.
	if maxLineRunes > maxRunesPerLine+20 {
		t.Errorf("per-line truncation failed: longest line has %d runes, cap is %d+20", maxLineRunes, maxRunesPerLine)
	}
}

// TestAdaptiveOutputResult_PureChinese_NoMojibake verifies that truncation is
// rune-safe: it never splits a multi-byte Chinese rune, which would produce
// invalid UTF-8 (mojibake). Before the fix, byte-based truncation could split
// a 3-byte Chinese rune at an arbitrary boundary.
//
// Backport from vx-toolkit extreme test E14 (pure Chinese 3000 lines).
func TestAdaptiveOutputResult_PureChinese_NoMojibake(t *testing.T) {
	chinese := strings.Repeat("中文字符测试", 200) // ~1200 runes per line, > maxRunesPerLine
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString(chinese + "\n")
	}
	data := []byte(sb.String())
	got := AdaptiveOutputResult(data, "test", "")
	if !utf8.Valid(got) {
		t.Errorf("mojibake detected: output contains invalid UTF-8 (rune splitting)")
	}
}

// TestSummarizeOutput_NeverBloat_TableSummary verifies that SummarizeOutput's
// never-bloat guard catches cases where a structured summary would be larger
// than the original. A table with exactly 3 rows (each ~3 KB) produces a
// summary that includes all 3 rows plus a header — slightly larger than the
// input. The guard returns the original instead.
func TestSummarizeOutput_NeverBloat_TableSummary(t *testing.T) {
	pad := strings.Repeat("x", 3000)
	var sb strings.Builder
	sb.WriteString("| " + pad + " | a | b |\n")
	sb.WriteString("| " + pad + " | c | d |\n")
	sb.WriteString("| " + pad + " | e | f |\n")
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes) to trigger summarization", len(data))
	}

	got := SummarizeOutput(data, "table")
	if len(got) > len(data) {
		t.Errorf("never-bloat violated: summary %d bytes > input %d bytes", len(got), len(data))
	}
}

// TestAdaptiveOutputResult_Determinism verifies that the same input always
// produces the same output (no non-deterministic behavior from map iteration
// or time-dependent logic in the truncation path).
//
// Backport from vx-toolkit extreme test F2 (determinism).
func TestAdaptiveOutputResult_Determinism(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("line " + itoa(i) + " content for testing determinism\n")
	}
	data := []byte(sb.String())
	got1 := string(AdaptiveOutputResult(data, "test", ""))
	got2 := string(AdaptiveOutputResult(data, "test", ""))
	if got1 != got2 {
		t.Errorf("non-deterministic: same input produced different outputs (len1=%d, len2=%d)", len(got1), len(got2))
	}
}

// TestAdaptiveOutputResult_NeverBloat_MediumBranch verifies the never-bloat
// guard in the medium-output branch (8 KB–128 KB, > 200 lines). With 201 lines
// where head(50)+tail(150) = 200 lines are shown plus markers, the result could
// exceed the original. The guard ensures output <= input.
func TestAdaptiveOutputResult_NeverBloat_MediumBranch(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 201; i++ {
		sb.WriteString("line " + itoa(i) + " padding padding padding padding padding padding\n")
	}
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes) to trigger summarization", len(data))
	}

	got := AdaptiveOutputResult(data, "test", "")
	if len(got) > len(data) {
		t.Errorf("never-bloat violated: output %d bytes > input %d bytes", len(got), len(data))
	}
}

// TestAdaptiveOutputResult_FewLinesLongLines_Truncated is a regression test for
// Gap 3: the medium-output branch (8 KB–128 KB, ≤ 200 lines) returned rawStdout
// verbatim, so a 12 KB input with 3 lines of 4 KB each (e.g. minified JSON or
// base64 blobs) passed through completely uncompressed. After the fix, each
// line is truncated to maxRunesPerLine runes, producing a much smaller output.
func TestAdaptiveOutputResult_FewLinesLongLines_Truncated(t *testing.T) {
	longLine := strings.Repeat("z", 4000) // 4 KB per line, > maxRunesPerLine
	data := []byte(longLine + "\n" + longLine + "\n" + longLine + "\n")
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes)", len(data))
	}
	if len(data) > 128*1024 {
		t.Fatalf("test data too large (%d bytes) — should be in medium branch", len(data))
	}

	got := AdaptiveOutputResult(data, "test", "")
	// Output should be significantly smaller than input (truncated).
	if len(got) >= len(data) {
		t.Errorf("Gap 3: few-lines-long-lines not truncated: output %d bytes >= input %d bytes", len(got), len(data))
	}
	// No line should exceed maxRunesPerLine + marker.
	for _, line := range strings.Split(string(got), "\n") {
		if utf8.RuneCountInString(line) > maxRunesPerLine+20 {
			t.Errorf("per-line truncation failed: line has %d runes > %d+20", utf8.RuneCountInString(line), maxRunesPerLine)
			break
		}
	}
}

// TestAdaptiveOutputResult_MidSectionSampling verifies Gap 4: when the omitted
// middle section has no error keywords, representative samples are included
// so the LLM has some visibility into the omitted content. Before the fix,
// the middle was completely hidden if no error keywords were found.
func TestAdaptiveOutputResult_MidSectionSampling(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 250; i++ {
		sb.WriteString("line " + itoa(i) + " unique content " + itoa(i*7) + " padding padding padding\n")
	}
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes)", len(data))
	}

	got := string(AdaptiveOutputResult(data, "test", ""))
	// With no error keywords, the middle samples should be present.
	if !strings.Contains(got, "Middle samples") {
		t.Errorf("Gap 4: expected middle samples in output, got:\n%s", got[:200])
	}
}

// TestWriteDumpIndex verifies Gap 6: dumpToWorkspaceTmp writes a dump_index.tsv
// file listing all cmd_dump_* files, so the LLM can discover dumps via read_file
// without needing to list the directory with a shell command.
func TestWriteDumpIndex(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a fake dump file.
	dumpFile := filepath.Join(tmpDir, "cmd_dump_12345_67890.txt")
	if err := os.WriteFile(dumpFile, []byte("test data"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	writeDumpIndex(tmpDir)

	indexPath := filepath.Join(tmpDir, "dump_index.tsv")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("Gap 6: dump_index.tsv not created: %v", err)
	}
	indexStr := string(indexData)
	if !strings.Contains(indexStr, "filename\ttimestamp\tsize_kb") {
		t.Errorf("Gap 6: index missing TSV header")
	}
	if !strings.Contains(indexStr, "cmd_dump_12345_67890.txt") {
		t.Errorf("Gap 6: index does not contain dump filename")
	}
}

// TestSummarizeOutput_LowThreshold_StructuredContent verifies Gap 7: structured
// content (JSON, table, log) is summarized at a 2 KB threshold instead of 8 KB,
// so small structured outputs don't accumulate uncompressed over a long session.
// A 3 KB JSON should be summarized, not passed through verbatim.
func TestSummarizeOutput_LowThreshold_StructuredContent(t *testing.T) {
	// 3 KB JSON — above the 2 KB structured threshold, below the 8 KB plain threshold.
	var sb strings.Builder
	sb.WriteString("{\n")
	for i := 0; i < 120; i++ {
		sb.WriteString("  \"key" + itoa(i) + "\": \"val" + itoa(i) + "\",\n")
	}
	sb.WriteString("  \"end\": true\n}\n")
	data := []byte(sb.String())
	if len(data) < 2*1024 {
		t.Fatalf("test data too small (%d bytes), need > 2 KB", len(data))
	}
	if len(data) >= 8*1024 {
		t.Fatalf("test data too large (%d bytes), need < 8 KB", len(data))
	}

	got := SummarizeOutput(data, "json")
	// Should be summarized, not passed through verbatim.
	if !strings.Contains(got, "JSON Summary") {
		t.Errorf("Gap 7: expected JSON summary for %d KB structured content, got: %s", len(data)/1024, got[:100])
	}
}

// ── MCP source-aware path tests ────────────────────────────────────────────

func TestAdaptiveOutputResult_MCP_Small_PassThrough(t *testing.T) {
	data := []byte(`{"content":[{"type":"text","text":"small result"}]}`)
	res := AdaptiveOutputResult(data, "search", "mcp:filesystem")
	if !bytes.Equal(res, data) {
		t.Errorf("MCP result <8KB should pass through unmodified")
	}
}

func TestAdaptiveOutputResult_MCP_Large_FileDump(t *testing.T) {
	// Create a >8KB MCP result.
	large := `{"content":[{"type":"text","text":"` + strings.Repeat("D", 10000) + `"}]}`
	data := []byte(large)
	res := AdaptiveOutputResult(data, "search", "mcp:filesystem")

	resStr := string(res)
	// Should NOT contain line-based CLI markers.
	if strings.Contains(resStr, "FIRST 30 LINES") {
		t.Errorf("MCP path should not use CLI line-based truncation")
	}
	if strings.Contains(resStr, "OUTPUT GATEWAY") {
		t.Errorf("MCP path should not use CLI output gateway format")
	}
	// Should contain MCP file-dump reference.
	if !strings.Contains(resStr, "MCP result") {
		t.Errorf("MCP large result should contain MCP result reference")
	}
	if !strings.Contains(resStr, "read_file") {
		t.Errorf("MCP large result should mention read_file for full access")
	}
}

func TestAdaptiveOutputResult_CLI_Source_UsesLineBasedPath(t *testing.T) {
	// CLI source should use the existing line-based path, not MCP path.
	// Data must be >8KB to trigger truncation.
	var buf bytes.Buffer
	for i := 0; i < 500; i++ {
		buf.WriteString(fmt.Sprintf("line %d: %s\n", i, strings.Repeat("x", 50)))
	}
	data := buf.Bytes()
	if len(data) < 8*1024 {
		t.Fatalf("test data must be >8KB, got %d bytes", len(data))
	}
	res := AdaptiveOutputResult(data, "run_command", "orchestrator")
	resStr := string(res)

	// Should NOT contain MCP-specific markers.
	if strings.Contains(resStr, "MCP result") {
		t.Errorf("CLI source should not use MCP path")
	}
	// Should contain CLI truncation markers.
	if !strings.Contains(resStr, "TRUNCATED") && !strings.Contains(resStr, "OUTPUT GATEWAY") {
		t.Errorf("CLI source should use line-based truncation")
	}
}

func TestAdaptiveOutputResult_EmptySource_UsesCLIPath(t *testing.T) {
	// Empty source should default to CLI path (not MCP).
	data := []byte(strings.Repeat("E", 10000))
	res := AdaptiveOutputResult(data, "test", "")
	resStr := string(res)

	if strings.Contains(resStr, "MCP result") {
		t.Errorf("Empty source should not use MCP path")
	}
}
