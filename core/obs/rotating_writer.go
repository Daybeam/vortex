package obs

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

type RotatingWriter struct {
	mu            sync.Mutex
	logDir        string
	filePath      string
	file          *os.File
	maxSize       int64
	currentSize   int64
	maxAgeDays    int
	totalCapBytes int64
	stderr        io.Writer
	wg            sync.WaitGroup
}

func NewRotatingWriter(logDir string, maxSizeMB, maxAgeDays, totalCapMB int) (*RotatingWriter, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}
	w := &RotatingWriter{
		logDir:        logDir,
		filePath:      filepath.Join(logDir, "app.log"),
		maxSize:       int64(maxSizeMB) * 1024 * 1024,
		maxAgeDays:    maxAgeDays,
		totalCapBytes: int64(totalCapMB) * 1024 * 1024,
		stderr:        os.Stderr,
	}
	if w.maxSize <= 0 {
		w.maxSize = 100 * 1024 * 1024
	}
	if w.maxAgeDays <= 0 {
		w.maxAgeDays = 7
	}
	if w.totalCapBytes <= 0 {
		w.totalCapBytes = 1024 * 1024 * 1024
	}

	f, err := os.OpenFile(w.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	w.file = f
	if info, err := f.Stat(); err == nil {
		w.currentSize = info.Size()
	}

	w.purge()

	return w, nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.currentSize+int64(len(p)) > w.maxSize {
		w.rotateLocked()
	}

	n, err := w.file.Write(p)
	if err != nil {
		return w.stderr.Write(p)
	}
	w.currentSize += int64(n)

	w.stderr.Write(p)

	return n, nil
}

func (w *RotatingWriter) rotateLocked() {
	if w.file != nil {
		w.file.Close()
	}

	timestamp := time.Now().Format("20060102-150405")
	rotatedPath := fmt.Sprintf("%s.%s", w.filePath, timestamp)

	var renamed bool
	for i := 0; i < 5; i++ {
		if err := os.Rename(w.filePath, rotatedPath); err == nil {
			renamed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !renamed {
		fmt.Fprintf(w.stderr, "[rotating-writer] Failed to rotate log %s (file may be locked)\n", w.filePath)
		f, err := os.OpenFile(w.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		w.file = f
		w.currentSize = 0
		return
	}

	f, err := os.OpenFile(w.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	w.file = f
	w.currentSize = 0

	w.wg.Add(1)
	go func(src string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[rotating-writer] goroutine panic: %v\n%s", r, debug.Stack())
			}
		}()
		defer w.wg.Done()

		gzPath := src + ".gz"
		if err := compressLogFile(src, gzPath); err == nil {
			os.Remove(src)
		} else {
			fmt.Fprintf(os.Stderr, "[rotating-writer] Failed to compress rotated log %s: %v\n", src, err)
		}
	}(rotatedPath)
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	w.mu.Unlock()
	w.wg.Wait()
	return nil
}

func (w *RotatingWriter) StartStderrCapture() func() {
	r, writeEnd, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(w.stderr, "[rotating-writer] Failed to create stderr pipe: %v\n", err)
		return func() {}
	}

	origStderr := os.Stderr
	os.Stderr = writeEnd

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		io.Copy(w, r)
		r.Close()
	}()

	return func() {
		os.Stderr = origStderr
		writeEnd.Close()
	}
}

func (w *RotatingWriter) purge() {
	entries, err := os.ReadDir(w.logDir)
	if err != nil {
		return
	}

	now := time.Now()
	threshold := now.AddDate(0, 0, -w.maxAgeDays)

	type gzEntry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var gzFiles []gzEntry
	var totalGzSize int64

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}

		name := entry.Name()
		fullPath := filepath.Join(w.logDir, name)

		if info.ModTime().Before(threshold) {
			os.Remove(fullPath)
			continue
		}

		if filepath.Ext(name) == ".gz" {
			gzFiles = append(gzFiles, gzEntry{
				path:  fullPath,
				size:  info.Size(),
				mtime: info.ModTime(),
			})
			totalGzSize += info.Size()
		}
	}

	if totalGzSize > w.totalCapBytes {
		sort.Slice(gzFiles, func(i, j int) bool {
			return gzFiles[i].mtime.Before(gzFiles[j].mtime)
		})
		for _, gz := range gzFiles {
			if totalGzSize <= w.totalCapBytes {
				break
			}
			os.Remove(gz.path)
			totalGzSize -= gz.size
		}
	}
}

func compressLogFile(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gzFile, err := os.Create(dst)
	if err != nil {
		return err
	}

	zw := gzip.NewWriter(gzFile)
	if _, err := io.Copy(zw, f); err != nil {
		gzFile.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		gzFile.Close()
		return err
	}
	return gzFile.Close()
}
