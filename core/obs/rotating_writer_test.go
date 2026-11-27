package obs

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRotatingWriter_BasicWrite verifies that data written to the
// RotatingWriter appears in the log file.
func TestRotatingWriter_BasicWrite(t *testing.T) {
	logDir := t.TempDir()
	w, err := NewRotatingWriter(logDir, 100, 7, 1024)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	data := []byte("hello rotating writer\n")
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(logDir, "app.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(content), "hello rotating writer") {
		t.Errorf("log file content = %q, want to contain %q", content, data)
	}
}

// TestRotatingWriter_RotationOnSize verifies that the log file rotates
// when it exceeds the maxSize threshold, and the rotated file is
// gzip-compressed asynchronously.
func TestRotatingWriter_RotationOnSize(t *testing.T) {
	logDir := t.TempDir()
	// 1 MB rotation threshold — small enough to trigger in a test.
	w, err := NewRotatingWriter(logDir, 1, 7, 1024)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	// Write enough data to trigger rotation. 1 MB of data with 1 MB threshold
	// means the second write should exceed the limit and trigger rotation.
	chunk := strings.Repeat("a", 512*1024) // 512 KB
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(chunk + "\n")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// Wait for async compression to produce a .gz file.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(logDir, "app.log.*.gz"))
		if len(matches) > 0 {
			return // Success: rotated file was compressed
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("no .gz file found after rotation; dir contents: %s", listDir(t, logDir))
}

// TestRotatingWriter_CloseWaitsForCompression verifies that Close()
// waits for async gzip compression to finish before returning.
func TestRotatingWriter_CloseWaitsForCompression(t *testing.T) {
	logDir := t.TempDir()
	w, err := NewRotatingWriter(logDir, 1, 7, 1024)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	// Write enough to trigger rotation.
	chunk := strings.Repeat("b", 600*1024) // 600 KB > 1 MB threshold after two writes
	w.Write([]byte(chunk + "\n"))
	w.Write([]byte(chunk + "\n"))

	// Close should wait for compression to finish.
	w.Close()

	// Verify .gz file exists — proves Close waited.
	matches, _ := filepath.Glob(filepath.Join(logDir, "app.log.*.gz"))
	if len(matches) == 0 {
		t.Fatalf("no .gz file found after Close(); dir: %s", listDir(t, logDir))
	}

	// Verify the .gz file is valid gzip.
	for _, gzPath := range matches {
		f, err := os.Open(gzPath)
		if err != nil {
			t.Fatalf("Open %s: %v", gzPath, err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("gzip.NewReader %s: %v", gzPath, err)
		}
		_, err = io.ReadAll(zr)
		f.Close()
		if err != nil {
			t.Fatalf("ReadAll gzip %s: %v", gzPath, err)
		}
		zr.Close()
	}
}

// TestRotatingWriter_PurgeOldFiles verifies that the purge function
// removes .gz files older than maxAgeDays.
func TestRotatingWriter_PurgeOldFiles(t *testing.T) {
	logDir := t.TempDir()

	// Create an old .gz file (8 days old, past the 7-day retention).
	oldGz := filepath.Join(logDir, "app.log.20260101-000000.gz")
	if err := os.WriteFile(oldGz, []byte("old"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Set its modification time to 8 days ago.
	oldTime := time.Now().AddDate(0, 0, -8)
	if err := os.Chtimes(oldGz, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Create a recent .gz file (1 day old, within retention).
	recentGz := filepath.Join(logDir, "app.log.20260107-000000.gz")
	if err := os.WriteFile(recentGz, []byte("recent"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	recentTime := time.Now().AddDate(0, 0, -1)
	if err := os.Chtimes(recentGz, recentTime, recentTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// NewRotatingWriter calls purge() on init.
	w, err := NewRotatingWriter(logDir, 100, 7, 1024)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	// Old file should be purged.
	if _, err := os.Stat(oldGz); !os.IsNotExist(err) {
		t.Errorf("old .gz file should have been purged, stat err = %v", err)
	}

	// Recent file should survive.
	if _, err := os.Stat(recentGz); err != nil {
		t.Errorf("recent .gz file should survive purge: %v", err)
	}
}

// TestRotatingWriter_PurgeByTotalSize verifies that the purge function
// removes the oldest .gz files when total size exceeds the cap.
func TestRotatingWriter_PurgeByTotalSize(t *testing.T) {
	logDir := t.TempDir()

	// Create two .gz files within the age window but exceeding the size cap.
	// totalCapMB=1 means 1 MB cap. We'll create files that total > 1 MB.
	// Use 512 KB each so two files = 1 MB, right at the cap. Use 3 files
	// to exceed it, forcing the oldest to be purged.
	data := strings.Repeat("z", 512*1024) // 512 KB

	for i, age := range []time.Duration{0, 0, 0} {
		name := filepath.Join(logDir, "app.log.2026010"+string(rune('1'+i))+"-000000.gz")
		if err := os.WriteFile(name, []byte(data), 0644); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
		// All within age window but with distinct mtimes so sort order is deterministic.
		mtime := time.Now().Add(-age).Add(-time.Duration(i) * time.Minute)
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			t.Fatalf("Chtimes %d: %v", i, err)
		}
	}

	// totalCapMB=1 (1 MB). Three 512 KB files = 1.5 MB > 1 MB cap.
	// The oldest file should be purged to bring total under cap.
	w, err := NewRotatingWriter(logDir, 100, 7, 1)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	matches, _ := filepath.Glob(filepath.Join(logDir, "*.gz"))
	// At least one file should have been purged (from 3 down to 2 or fewer).
	if len(matches) >= 3 {
		t.Errorf("expected size-based purge to remove at least one .gz file, got %d files: %v", len(matches), matches)
	}
}

// TestRotatingWriter_DualWrite verifies that writes go to both the log
// file and the original stderr (dual-write).
func TestRotatingWriter_DualWrite(t *testing.T) {
	logDir := t.TempDir()

	// Capture original stderr.
	origStderr := os.Stderr
	r, w2, _ := os.Pipe()
	os.Stderr = w2
	t.Cleanup(func() {
		os.Stderr = origStderr
		w2.Close()
	})

	w, err := NewRotatingWriter(logDir, 100, 7, 1024)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	// Write data — should appear in both the file and the pipe (stderr).
	testData := []byte("dual-write test\n")
	w.Write(testData)

	// Restore stderr and read from the pipe.
	os.Stderr = origStderr
	w2.Close()
	stderrBuf := make([]byte, 1024)
	n, _ := r.Read(stderrBuf)
	r.Close()

	if !strings.Contains(string(stderrBuf[:n]), "dual-write test") {
		t.Errorf("stderr output = %q, want to contain %q", stderrBuf[:n], testData)
	}

	// Also verify the file has the data.
	content, _ := os.ReadFile(filepath.Join(logDir, "app.log"))
	if !strings.Contains(string(content), "dual-write test") {
		t.Errorf("file content = %q, want to contain %q", content, testData)
	}
}

// TestRotatingWriter_StderrCapture verifies that StartStderrCapture
// redirects os.Stderr writes to the RotatingWriter.
func TestRotatingWriter_StderrCapture(t *testing.T) {
	logDir := t.TempDir()
	w, err := NewRotatingWriter(logDir, 100, 7, 1024)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	cleanup := w.StartStderrCapture()
	t.Cleanup(cleanup)

	// Write to os.Stderr — should be captured to the log file.
	fmt.Fprintf(os.Stderr, "captured stderr message\n")

	// Give the pipe reader goroutine time to process.
	time.Sleep(100 * time.Millisecond)

	content, err := os.ReadFile(filepath.Join(logDir, "app.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(content), "captured stderr message") {
		t.Errorf("log file = %q, want to contain captured stderr message", content)
	}
}

// TestCompressLogFile verifies that compressLogFile produces a valid
// gzip file containing the original data.
func TestCompressLogFile(t *testing.T) {
	logDir := t.TempDir()
	src := filepath.Join(logDir, "source.log")
	dst := filepath.Join(logDir, "source.log.gz")

	original := []byte("test data for compression\nline 2\nline 3\n")
	if err := os.WriteFile(src, original, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := compressLogFile(src, dst); err != nil {
		t.Fatalf("compressLogFile: %v", err)
	}

	// Read back the gzip file.
	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()

	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(decompressed) != string(original) {
		t.Errorf("decompressed = %q, want %q", decompressed, original)
	}
}

// listDir is a test helper that returns a string listing of a directory
// for debugging output in test failures.
func listDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	var names []string
	for _, e := range entries {
		info, _ := e.Info()
		names = append(names, e.Name()+" ("+fmt.Sprintf("%d", info.Size())+" bytes)")
	}
	return strings.Join(names, ", ")
}
