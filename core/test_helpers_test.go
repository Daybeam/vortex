package core

import (
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// mustNewLogger creates a Logger or fails the test immediately.
// Use this in tests to avoid repeating the error-check boilerplate.
//
// Usage:
//
//	logger := mustNewLogger(t, dir, &config.SystemSettings{})
//	defer logger.Close()
func mustNewLogger(t *testing.T, dir string, cfg *config.SystemSettings) *Logger {
	t.Helper()
	if dir == "" {
		tmp, err := os.MkdirTemp("", "vortex-test-logger-*")
		if err != nil {
			t.Fatalf("MkdirTemp failed: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(tmp) })
		dir = tmp
	}
	logger, err := NewLogger(dir, cfg)
	if err != nil {
		t.Fatalf("NewLogger(%q, ...) failed: %v", dir, err)
	}
	return logger
}

// mustNewExperienceStore creates an ExperienceStore or fails the test immediately.
// Use this in tests to avoid repeating the error-check boilerplate.
//
// Usage:
//
//	es := mustNewExperienceStore(t, dir, ts, &config.SystemSettings{}, nil, nil)
func mustNewExperienceStore(
	t *testing.T,
	dir string,
	ts store.ITaskStore,
	sys *config.SystemSettings,
	expBackend store.IExperienceBackend,
	apBackend store.IAntiPatternBackend,
) *store.ExperienceStore {
	t.Helper()
	es, err := store.NewExperienceStore(dir, ts, sys, expBackend, apBackend)
	if err != nil {
		t.Fatalf("NewExperienceStore(%q, ...) failed: %v", dir, err)
	}
	return es
}

// mustTempDir creates a temp dir that is cleaned up after the test.
// Use this instead of t.TempDir() when background goroutines write to the
// dir asynchronously — t.TempDir()'s RemoveAll fails on non-empty dirs.
func mustTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
