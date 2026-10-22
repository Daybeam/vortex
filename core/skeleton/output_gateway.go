package skeleton

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxRunesPerLine caps each line included in the gateway output. This prevents
// a single pathological line (e.g. a 10 MB minified bundle or base64 blob) from
// blowing up the context window. 1000 runes ≈ 3 KB UTF-8 — generous enough for
// any normal log line, stack trace, or table row.
const maxRunesPerLine = 1000

// truncateLineRuneSafe truncates s to at most maxRunes Unicode code points,
// appending "…[truncated]" if truncation occurred. Rune-safe: never splits a
// multi-byte sequence (prevents mojibake in CJK text).
func truncateLineRuneSafe(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…[truncated]"
}

// joinTruncated joins lines with "\n" after truncating each to maxRunes.
// This prevents a single pathological line from blowing up the output.
func joinTruncated(lines []string, maxRunes int) string {
	trunc := make([]string, len(lines))
	for i, l := range lines {
		trunc[i] = truncateLineRuneSafe(l, maxRunes)
	}
	return strings.Join(trunc, "\n")
}

// neverBloatGuard returns result if it is smaller than raw, otherwise returns
// raw. This ensures the gateway never inflates the input — a summary that is
// larger than the original is worse than useless.
func neverBloatGuard(result, raw []byte) []byte {
	if len(result) >= len(raw) {
		return raw
	}
	return result
}

// AdaptiveOutputResult handles multi-platform size checking, truncation, and fallback disk-dump.
// The source parameter selects the truncation strategy:
//   - "mcp:*" sources: JSON-aware MCP path (file-dump for >8KB, no line-based
//     head/tail, no CLI error keywords — preserves JSON structure)
//   - other sources: CLI path (line-based head/tail, error keyword extraction,
//     cmd_dump_* file naming — designed for run_command stdout)
func AdaptiveOutputResult(rawStdout []byte, command string, source string) []byte {
	// MCP JSON-RPC results: use a structure-preserving path.
	if strings.HasPrefix(source, "mcp:") {
		return adaptiveOutputResultMCP(rawStdout, command)
	}
	return adaptiveOutputResultCLI(rawStdout, command)
}

// adaptiveOutputResultMCP handles MCP tool results (JSON-RPC).
// Unlike the CLI path, it does NOT do line-based head/tail (which breaks JSON)
// or CLI error keyword extraction (panic:, Traceback — irrelevant for JSON-RPC).
// For results >8KB, it side-loads to a file so the model can read_file the
// full content if needed.
func adaptiveOutputResultMCP(raw []byte, command string) []byte {
	if len(raw) < 8*1024 {
		return raw // Gate 1 already handled truncation
	}
	dumpPath := dumpToWorkspaceTmp(raw, command)
	if dumpPath == "" {
		// Dump failed: don't advertise a path the model cannot read — it costs a turn.
		return []byte(fmt.Sprintf(
			"[MCP result: %d KB. dump unavailable — the full body could not be persisted; re-run with a narrower filter to see it in context.]",
			len(raw)/1024))
	}
	return []byte(fmt.Sprintf(
		"[MCP result: %d KB. Full content saved to: %s]\n[Use read_file to access full content.]",
		len(raw)/1024, dumpPath))
}

// adaptiveOutputResultCLI handles CLI command output (run_command stdout).
// This is the original line-based strategy: head/tail by lines, error keyword
// extraction, cmd_dump_* file naming. Designed for semi-structured terminal output.
func adaptiveOutputResultCLI(rawStdout []byte, command string) []byte {
	rawStr := string(rawStdout)
	normalized := strings.ReplaceAll(rawStr, "\r\n", "\n")
	outputBytes := len(rawStdout)

	if outputBytes < 8*1024 {
		return rawStdout
	}

	lines := strings.Split(normalized, "\n")
	totalLines := len(lines)

	if outputBytes > 128*1024 || totalLines > 2000 {
		dumpPath := dumpToWorkspaceTmp(rawStdout, command)
		headCount := 30
		tailCount := 30
		if totalLines <= headCount+tailCount {
			// Few lines but large bytes → truncate each line to prevent bloat
			// from a single pathological line (e.g. 10 MB minified bundle).
			truncLines := make([]string, len(lines))
			for i, l := range lines {
				truncLines[i] = truncateLineRuneSafe(l, maxRunesPerLine)
			}
			result := fmt.Sprintf("[Output large: %d KB / %d lines. Full log saved to: %s]\n%s",
				outputBytes/1024, totalLines, dumpPath, strings.Join(truncLines, "\n"))
			if dumpPath == "" {
				result = fmt.Sprintf("[Output large: %d KB / %d lines. dump unavailable — the log could not be persisted.]\n%s",
					outputBytes/1024, totalLines, strings.Join(truncLines, "\n"))
			}
			return neverBloatGuard([]byte(result), rawStdout)
		}

		head := joinTruncated(lines[:headCount], maxRunesPerLine)
		tail := joinTruncated(lines[totalLines-tailCount:], maxRunesPerLine)

		summary := fmt.Sprintf(
			"[⚖️ OUTPUT GATEWAY: Output too large (%d KB, %d lines). Full content persisted to: %s]\n"+
				"--- FIRST 30 LINES ---\n%s\n\n... [%d lines / %d KB omitted, checking for panic/error keywords] ...\n\n--- LAST 30 LINES ---\n%s",
			outputBytes/1024, totalLines, dumpPath, head, totalLines-headCount-tailCount, (outputBytes-len(head)-len(tail))/1024, tail,
		)
		if dumpPath == "" {
			summary = fmt.Sprintf(
				"[⚖️ OUTPUT GATEWAY: Output too large (%d KB, %d lines). dump unavailable — the log could not be persisted.]\n"+
					"--- FIRST 30 LINES ---\n%s\n\n... [%d lines / %d KB omitted, checking for panic/error keywords] ...\n\n--- LAST 30 LINES ---\n%s",
				outputBytes/1024, totalLines, head, totalLines-headCount-tailCount, (outputBytes-len(head)-len(tail))/1024, tail,
			)
		}
		return neverBloatGuard([]byte(summary), rawStdout)
	}

	headCount := 50
	tailCount := 150
	if totalLines <= headCount+tailCount {
		// Few lines but potentially large bytes → truncate each line to prevent
		// a single pathological line (e.g. 50 KB minified JSON) from passing
		// through uncompressed. If no line exceeds the cap, joinTruncated
		// returns the same content and neverBloatGuard returns rawStdout.
		return neverBloatGuard([]byte(joinTruncated(lines, maxRunesPerLine)), rawStdout)
	}

	head := joinTruncated(lines[:headCount], maxRunesPerLine)
	tail := joinTruncated(lines[totalLines-tailCount:], maxRunesPerLine)
	omittedLines := totalLines - headCount - tailCount

	omittedZone := strings.Join(lines[headCount:totalLines-tailCount], "\n")
	extraSnippet := extractErrorKeywords(omittedZone)
	// If no error keywords found, sample representative lines from the middle
	// to give the LLM some visibility into the omitted section.
	if extraSnippet == "" {
		extraSnippet = sampleMiddleLines(lines[headCount:totalLines-tailCount], maxRunesPerLine)
	}

	resultStr := fmt.Sprintf(
		"[⚠️ OUTPUT TRUNCATED: %d lines total. Showing first %d and last %d lines.]\n%s\n\n... [%d lines omitted%s] ...\n\n%s",
		totalLines, headCount, tailCount, head, omittedLines, extraSnippet, tail,
	)
	return neverBloatGuard([]byte(resultStr), rawStdout)
}

// dumpRetentionTTL is the maximum age of cmd_dump_* files before lazy cleanup
// removes them. 24h is sufficient: dumps exist only for the LLM to re-read
// mid-task via read_file; tasks rarely span more than a few hours.
const dumpRetentionTTL = 24 * time.Hour

// maxDumpsToRetain caps the total number of dump files kept. This prevents
// unbounded accumulation when many large outputs are dumped in quick succession
// (e.g. a noisy test suite run) before the TTL-based cleanup fires.
const maxDumpsToRetain = 50

func dumpToWorkspaceTmp(data []byte, command string) string {
	tmpDir := "workspace/tmp"
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		tmpDir = os.TempDir()
	}

	// Lazy cleanup: remove stale dumps before writing a new one.
	// This is called infrequently (only when output > 128KB), so scanning
	// the directory here is cheaper than running a background goroutine.
	cleanupStaleDumps(tmpDir)

	filename := fmt.Sprintf("cmd_dump_%d_%d.txt", time.Now().UnixNano(), os.Getpid())
	fullPath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		// FIX 2026-10-20: return "" so callers can produce an honest "dump
		// unavailable" message instead of advertising a path to a file that
		// does not exist — the model would waste a turn trying to read it.
		log.Printf("WARN: dumpToWorkspaceTmp: failed to write %s: %v", fullPath, err)
		return ""
	}
	absPath, _ := filepath.Abs(fullPath)
	writeDumpIndex(tmpDir)
	return absPath
}

// writeDumpIndex writes a TSV index of all cmd_dump_* files in dir.
// This lets the LLM discover and read dump files via read_file, without
// needing to list the directory with a shell command.
func writeDumpIndex(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var rows []string
	rows = append(rows, "filename\ttimestamp\tsize_kb")
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cmd_dump_") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s\t%d\t%d",
			e.Name(), info.ModTime().Unix(), info.Size()/1024))
	}
	indexPath := filepath.Join(dir, "dump_index.tsv")
	_ = os.WriteFile(indexPath, []byte(strings.Join(rows, "\n")), 0644)
}

// cleanupStaleDumps removes cmd_dump_* files older than dumpRetentionTTL.
// If files remain after TTL-based removal, the oldest are pruned until
// the count drops to maxDumpsToRetain.
func cleanupStaleDumps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // dir doesn't exist yet — nothing to clean
	}

	type dumpInfo struct {
		name    string
		modTime time.Time
	}

	var dumps []dumpInfo
	now := time.Now()

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "cmd_dump_") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// TTL eviction: remove files older than dumpRetentionTTL.
		if now.Sub(info.ModTime()) > dumpRetentionTTL {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		dumps = append(dumps, dumpInfo{name: name, modTime: info.ModTime()})
	}

	// Count eviction: if still over maxDumpsToRetain, prune oldest first.
	if len(dumps) <= maxDumpsToRetain {
		return
	}
	// Sort by modTime ascending (oldest first).
	sort.Slice(dumps, func(i, j int) bool {
		return dumps[i].modTime.Before(dumps[j].modTime)
	})
	excess := len(dumps) - maxDumpsToRetain
	for i := 0; i < excess; i++ {
		_ = os.Remove(filepath.Join(dir, dumps[i].name))
	}
}

func extractErrorKeywords(text string) string {
	keywords := []string{"panic:", "error", "fatal", "Traceback", "FAILED", "Exception"}
	var found []string
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				found = append(found, "  ↳ Match: "+truncateLineRuneSafe(strings.TrimSpace(line), maxRunesPerLine))
				break
			}
		}
		if len(found) >= 5 {
			break
		}
	}
	if len(found) > 0 {
		return " | Found critical hints:\n" + strings.Join(found, "\n")
	}
	return ""
}

// sampleMiddleLines picks up to 3 representative lines from the omitted zone
// (at 25%, 50%, 75% positions). This gives the LLM a glimpse of the middle
// content even when no error keywords are found, preventing total blindness
// about the omitted section.
func sampleMiddleLines(lines []string, maxRunes int) string {
	if len(lines) == 0 {
		return ""
	}
	positions := []float64{0.25, 0.5, 0.75}
	var samples []string
	for _, p := range positions {
		idx := int(float64(len(lines)) * p)
		if idx >= len(lines) {
			idx = len(lines) - 1
		}
		line := strings.TrimSpace(lines[idx])
		if line == "" {
			continue
		}
		samples = append(samples, "  ↳ Sample: "+truncateLineRuneSafe(line, maxRunes))
	}
	if len(samples) == 0 {
		return ""
	}
	return " | Middle samples:\n" + strings.Join(samples, "\n")
}
