package core

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
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

func dumpToWorkspaceTmp(data []byte, command string) string {
	tmpDir := "workspace/tmp"
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		tmpDir = os.TempDir()
	}

	filename := fmt.Sprintf("cmd_dump_%d_%d.txt", time.Now().UnixNano(), os.Getpid())
	fullPath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		// Audit: was silently swallowed — caller would get a path to a non-existent file.
		log.Printf("WARN: dumpToWorkspaceTmp: failed to write %s: %v", fullPath, err)
	}
	absPath, _ := filepath.Abs(fullPath)
	return absPath
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
