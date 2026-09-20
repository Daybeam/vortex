package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReplacementItem represents a single text replacement operation.
type ReplacementItem struct {
	FilePath string `json:"file_path"`
	OldStr   string `json:"old_str"`
	NewStr   string `json:"new_str"`
}

// BatchReplaceRequest is the input for ExecuteBatchReplace.
type BatchReplaceRequest struct {
	Replacements []ReplacementItem `json:"replacements"`
}

// BatchReplaceResult is the output of ExecuteBatchReplace.
type BatchReplaceResult struct {
	Success       bool     `json:"success"`
	ModifiedFiles []string `json:"modified_files"`
	Summary       string   `json:"summary"`
}

// ExecuteBatchReplace applies multiple text replacements across files atomically.
// It validates all replacements first (existence, uniqueness, non-overlap),
// stages them in temp files, then commits via atomic rename. On any failure,
// all changes are rolled back and temp files are cleaned up.
//
// Design: docs/architecture/REPLACE_BATCH_DESIGN.md
func ExecuteBatchReplace(req BatchReplaceRequest) (*BatchReplaceResult, error) {
	if len(req.Replacements) == 0 {
		return nil, fmt.Errorf("no replacements provided")
	}

	// Group replacements by file path (preserve first-seen order)
	fileEdits := make(map[string][]ReplacementItem)
	var fileOrder []string
	for _, item := range req.Replacements {
		if !filepath.IsAbs(item.FilePath) {
			return nil, fmt.Errorf("file_path must be absolute: %s", item.FilePath)
		}
		if _, exists := fileEdits[item.FilePath]; !exists {
			fileOrder = append(fileOrder, item.FilePath)
		}
		fileEdits[item.FilePath] = append(fileEdits[item.FilePath], item)
	}

	// Snapshots for rollback (only populated for files that were read)
	originals := make(map[string][]byte)
	tempFiles := make(map[string]string)

	// Phase 1: Read, Validate, and Stage per file
	for _, path := range fileOrder {
		items := fileEdits[path]

		contentBytes, err := os.ReadFile(path)
		if err != nil {
			cleanupTempFiles(tempFiles)
			return nil, fmt.Errorf("failed to read file %s: %w", path, err)
		}
		originals[path] = contentBytes
		content := string(contentBytes)

		// Find all replacement offsets in the ORIGINAL content and validate
		type editSpan struct {
			start int
			end   int
			item  ReplacementItem
		}
		var spans []editSpan
		for _, edit := range items {
			if edit.OldStr == "" {
				cleanupTempFiles(tempFiles)
				return nil, fmt.Errorf("empty old_str for file %s", path)
			}
			count := strings.Count(content, edit.OldStr)
			if count == 0 {
				cleanupTempFiles(tempFiles)
				return nil, fmt.Errorf("old_str not found in file %s: %q", path, edit.OldStr)
			}
			if count > 1 {
				cleanupTempFiles(tempFiles)
				return nil, fmt.Errorf("old_str is not unique in file %s (found %d times)", path, count)
			}
			idx := strings.Index(content, edit.OldStr)
			spans = append(spans, editSpan{start: idx, end: idx + len(edit.OldStr), item: edit})
		}

		// Check non-overlap: sort by start offset, verify no span overlaps its predecessor
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		for i := 1; i < len(spans); i++ {
			if spans[i].start < spans[i-1].end {
				cleanupTempFiles(tempFiles)
				return nil, fmt.Errorf("overlapping replacements in file %s at offset %d", path, spans[i].start)
			}
		}

		// Apply replacements from bottom to top (highest offset first)
		// so earlier offsets aren't invalidated by length changes.
		newContent := content
		for i := len(spans) - 1; i >= 0; i-- {
			s := spans[i]
			newContent = newContent[:s.start] + s.item.NewStr + newContent[s.end:]
		}

		// Write to temp file in same directory for atomic rename
		randBytes := make([]byte, 4)
		_, _ = rand.Read(randBytes)
		tmpPath := path + "." + hex.EncodeToString(randBytes) + ".tmp"

		if err := os.WriteFile(tmpPath, []byte(newContent), 0644); err != nil {
			cleanupTempFiles(tempFiles)
			return nil, fmt.Errorf("failed to write staging file %s: %w", tmpPath, err)
		}
		tempFiles[path] = tmpPath
	}

	// Phase 2: Commit (Atomic Rename)
	var modified []string
	for _, path := range fileOrder {
		tmpPath := tempFiles[path]
		if err := os.Rename(tmpPath, path); err != nil {
			// Rollback already-committed files from originals
			for _, committed := range modified {
				if orig, ok := originals[committed]; ok {
					_ = os.WriteFile(committed, orig, 0644)
				}
			}
			// Clean up remaining uncommitted temp files
			for p, tp := range tempFiles {
				if p != path {
					_ = os.Remove(tp)
				}
			}
			return nil, fmt.Errorf("failed to atomically commit file %s: %w", path, err)
		}
		delete(tempFiles, path)
		modified = append(modified, path)
	}

	return &BatchReplaceResult{
		Success:       true,
		ModifiedFiles: modified,
		Summary:       fmt.Sprintf("Successfully applied %d replacements across %d files.", len(req.Replacements), len(fileOrder)),
	}, nil
}

// cleanupTempFiles removes all temporary staging files.
func cleanupTempFiles(tempFiles map[string]string) {
	for _, tmpPath := range tempFiles {
		_ = os.Remove(tmpPath)
	}
}
