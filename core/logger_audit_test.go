package core

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// -----------------------------------------------------------------------------
// Regression test for audit LE-1 (2026-10-24): gzip compression data loss
// in Logger.compressFile. The function deferred gzFile.Close() and zw.Close()
// without checking errors. gzip.Writer.Close() writes the gzip footer
// (CRC + size); if it fails (disk full, I/O error), the gzip file is corrupt
// but compressFile returned nil. The caller then deleted the source log,
// causing permanent data loss.
//
// Before fix: close errors were swallowed (defer without check).
// After fix:  explicit ordered closes with error propagation.
// Source: docs/archive/audits/AUDIT_REPORT_2026-10-24_COMPREHENSIVE.md §LE-1
// -----------------------------------------------------------------------------

// TestCompressFile_HappyPath verifies that compressFile produces a valid gzip
// archive that can be decompressed to match the original. This confirms the
// close ordering (zw.Close before gzFile.Close) is correct after the fix.
func TestCompressFile_HappyPath(t *testing.T) {
	logDir := t.TempDir()
	logger, err := NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	defer logger.Close()

	// Create a source file with known content.
	content := []byte("line 1: test log data\nline 2: more data\nline 3: end\n")
	srcPath := filepath.Join(logDir, "source.jsonl")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Compress the file.
	dstPath := filepath.Join(logDir, "source.jsonl.gz")
	if err := logger.compressFile(srcPath, dstPath); err != nil {
		t.Fatalf("compressFile failed: %v", err)
	}

	// Decompress and verify content matches.
	gzFile, err := os.Open(dstPath)
	if err != nil {
		t.Fatalf("cannot open compressed file: %v", err)
	}
	defer gzFile.Close()

	zr, err := gzip.NewReader(gzFile)
	if err != nil {
		t.Fatalf("gzip.NewReader failed (corrupt archive?): %v", err)
	}
	defer zr.Close()

	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("io.ReadAll failed: %v", err)
	}

	if !bytes.Equal(decompressed, content) {
		t.Errorf("decompressed content does not match original\nwant: %q\ngot:  %q", content, decompressed)
	}
}

// TestCompressFile_ErrorOnMissingSource verifies that compressFile returns an
// error (not nil) when the source file doesn't exist. Before the fix, close
// errors were swallowed; after the fix, all error paths propagate.
func TestCompressFile_ErrorOnMissingSource(t *testing.T) {
	logDir := t.TempDir()
	logger, err := NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	defer logger.Close()

	srcPath := filepath.Join(logDir, "nonexistent.jsonl")
	dstPath := filepath.Join(logDir, "output.gz")

	if err := logger.compressFile(srcPath, dstPath); err == nil {
		t.Fatal("compressFile should return error for missing source, got nil")
	}
}

// TestCompressFile_ErrorOnInvalidDestination verifies that compressFile
// returns an error when the destination directory doesn't exist.
func TestCompressFile_ErrorOnInvalidDestination(t *testing.T) {
	logDir := t.TempDir()
	logger, err := NewLogger(logDir, &config.SystemSettings{})
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	defer logger.Close()

	// Create a valid source file.
	srcPath := filepath.Join(logDir, "source.jsonl")
	if err := os.WriteFile(srcPath, []byte("test data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Destination in a non-existent directory.
	dstPath := filepath.Join(logDir, "nonexistent_subdir", "output.gz")

	if err := logger.compressFile(srcPath, dstPath); err == nil {
		t.Fatal("compressFile should return error for invalid destination, got nil")
	}
}
