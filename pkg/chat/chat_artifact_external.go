package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/core/skeleton"
)

const artifactThreshold = 2 * 1024 // 2 KB

const readPassthroughCeiling = 32 * 1024

func (h *ChatHarness) shouldExternalize(toolName, result string) bool {
	if h.OutputBase == "" || len(result) <= artifactThreshold {
		return false
	}
	if sieve.IsReadTool(toolName) {
		return len(result) > readPassthroughCeiling
	}
	return true
}

func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (h *ChatHarness) externalizeArtifact(result, toolName, taskID string) string {
	if h.OutputBase == "" {
		return result
	}
	artDir := filepath.Join(h.OutputBase, taskID, "artifacts")
	if err := os.MkdirAll(artDir, 0755); err != nil {
		return result
	}
	filename := fmt.Sprintf("art_%d_%s.txt", time.Now().UnixNano(), sanitizeToolName(toolName))
	fullPath := filepath.Join(artDir, filename)
	if err := os.WriteFile(fullPath, []byte(result), 0644); err != nil {
		return result
	}
	absPath, _ := filepath.Abs(fullPath)

	skel := skeleton.DetectAndSkeletonize([]byte(result), toolName)

	if len(skel) >= len(result) {
		if alt := skeleton.SummarizeOutput([]byte(result), toolName); len(alt) < len(result) {
			skel = alt
		} else {
			skel = fmt.Sprintf("[%d bytes, %d lines; skeleton omitted because it would not be smaller than the content]",
				len(result), strings.Count(result, "\n")+1)
		}
	}

	if h.Archive != nil {
		summary := skel
		if len(summary) > 500 {
			summary = summary[:500] + "\n... (skeleton truncated, see file)"
		}
		if aerr := h.Archive.Append(core.MemoryItem{
			TaskID:     taskID,
			NodeID:     fmt.Sprintf("artifact_%s_%d", taskID, time.Now().UnixNano()),
			Intent:     fmt.Sprintf("[artifact: %s]", toolName),
			Summary:    summary,
			SourceKeys: []string{absPath},
			Timestamp:  time.Now(),
		}); aerr != nil {
			fmt.Fprintf(os.Stderr, "[chat-harness] WARN: failed to index artifact in core.ContextArchive: %v\n", aerr)
		}
	}

	return fmt.Sprintf(
		"[Artifact externalized: %d KB. Skeleton below. Full content at: %s]\n%s\n"+
			"[Use read_file on that path to read the full content; add start_line/end_line to page through a large file.]",
		len(result)/1024, absPath, skel,
	)
}
