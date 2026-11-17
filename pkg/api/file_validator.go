package api

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ── File Upload Security ──────────────────────────────────────────────────
//
// This file provides validation for uploaded files before they are stored
// in the session workspace. The validation is intentionally minimal:
//   1. File size limit (MaxUploadFileSize)
//   2. Extension whitelist (allowedUploadExts)
//   3. Filename sanitization (prevent path traversal)
//   4. Pluggable content-scanning hook (FileContentValidator)
//
// The hook point (FileContentValidator) is reserved for future content
// scanning (e.g., virus scan, embedded script detection). It is nil by
// default — set it at startup if content scanning is needed.

// MaxUploadFileSize is the per-file size limit (10 MB).
const MaxUploadFileSize = 10 * 1024 * 1024

// allowedUploadExts is the whitelist of accepted file extensions.
var allowedUploadExts = map[string]bool{
	".txt": true, ".json": true, ".csv": true, ".md": true,
	".py": true, ".js": true, ".ts": true, ".go": true,
	".yaml": true, ".yml": true, ".pdf": true, ".xml": true,
	".html": true, ".css": true, ".sh": true, ".sql": true,
	".log": true, ".env": true, ".toml": true, ".ini": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true,
}

// FileContentValidator is a pluggable hook for content-level file validation.
// It receives the file path on disk after the file has been written, and
// returns an error if the content is rejected. nil means no content scanning.
//
// For thread-safe access after startup, use SetFileContentValidator and
// getFileContentValidator. Direct assignment is safe only before goroutines
// start (audit L-MED-8).
//
// Example usage at startup:
//
//	api.SetFileContentValidator(func(path string) error {
//	    return clamav.Scan(path) // or any content scanner
//	})
var (
	fileContentValidatorMu sync.RWMutex
	FileContentValidator   func(path string) error
)

// SetFileContentValidator sets the content validation hook (thread-safe).
func SetFileContentValidator(f func(path string) error) {
	fileContentValidatorMu.Lock()
	defer fileContentValidatorMu.Unlock()
	FileContentValidator = f
}

// getFileContentValidator returns the current content validation hook.
func getFileContentValidator() func(path string) error {
	fileContentValidatorMu.RLock()
	defer fileContentValidatorMu.RUnlock()
	return FileContentValidator
}

// sanitizeFilename strips path components and dangerous characters from a
// filename, preventing path traversal (../../etc/passwd) and null bytes.
func sanitizeFilename(name string) string {
	// Take only the base name (strips any directory components)
	name = filepath.Base(name)
	// Remove null bytes and control characters
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1 // drop control chars
		}
		return r
	}, name)
	// Collapse dots to prevent hidden files like .. or ...
	// but keep extension dots (only collapse leading dots)
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "unnamed"
	}
	return name
}

// validateUploadFile checks the filename and size of an uploaded file.
// It returns the sanitized filename and an error if validation fails.
func validateUploadFile(filename string, size int64) (string, error) {
	// 1. Size check
	if size > MaxUploadFileSize {
		return "", fmt.Errorf("file size %d bytes exceeds limit %d bytes (%.1f MB)",
			size, MaxUploadFileSize, float64(MaxUploadFileSize)/(1024*1024))
	}
	if size == 0 {
		return "", fmt.Errorf("file is empty")
	}

	// 2. Sanitize filename
	safeName := sanitizeFilename(filename)

	// 3. Extension whitelist
	ext := strings.ToLower(filepath.Ext(safeName))
	if ext == "" {
		return "", fmt.Errorf("file has no extension")
	}
	if !allowedUploadExts[ext] {
		return "", fmt.Errorf("file extension %q is not allowed", ext)
	}

	return safeName, nil
}
