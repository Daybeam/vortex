package core

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AdaptiveOutputResult handles multi-platform size checking, truncation, and fallback disk-dump.
func AdaptiveOutputResult(rawStdout []byte, command string) []byte {
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
			return []byte(fmt.Sprintf("[Output large: %d KB / %d lines. Full log saved to: %s]\n%s",
				outputBytes/1024, totalLines, dumpPath, normalized))
		}

		head := strings.Join(lines[:headCount], "\n")
		tail := strings.Join(lines[totalLines-tailCount:], "\n")

		summary := fmt.Sprintf(
			"[⚖️ OUTPUT GATEWAY: Output too large (%d KB, %d lines). Full content persisted to: %s]\n"+
				"--- FIRST 30 LINES ---\n%s\n\n... [%d lines / %d KB omitted, checking for panic/error keywords] ...\n\n--- LAST 30 LINES ---\n%s",
			outputBytes/1024, totalLines, dumpPath, head, totalLines-headCount-tailCount, (outputBytes-len(head)-len(tail))/1024, tail,
		)
		return []byte(summary)
	}

	headCount := 50
	tailCount := 150
	if totalLines <= headCount+tailCount {
		return rawStdout
	}

	head := strings.Join(lines[:headCount], "\n")
	tail := strings.Join(lines[totalLines-tailCount:], "\n")
	omittedLines := totalLines - headCount - tailCount

	omittedZone := strings.Join(lines[headCount:totalLines-tailCount], "\n")
	extraSnippet := extractErrorKeywords(omittedZone)

	resultStr := fmt.Sprintf(
		"[⚠️ OUTPUT TRUNCATED: %d lines total. Showing first %d and last %d lines.]\n%s\n\n... [%d lines omitted%s] ...\n\n%s",
		totalLines, headCount, tailCount, head, omittedLines, extraSnippet, tail,
	)
	return []byte(resultStr)
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
		// Audit: was silently swallowed — caller would get a path to a non-existent file.
		log.Printf("WARN: dumpToWorkspaceTmp: failed to write %s: %v", fullPath, err)
	}
	absPath, _ := filepath.Abs(fullPath)
	return absPath
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
				found = append(found, "  ↳ Match: "+strings.TrimSpace(line))
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
