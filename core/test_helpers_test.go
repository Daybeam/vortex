package core

import (
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// mustTempDir creates a temporary directory or fails the test immediately.
// The directory is automatically removed when the test finishes via t.Cleanup.
//
// Usage:
//
//	dir := mustTempDir(t, "debate-log")
func mustTempDir(t *testing.T, prefix string) string {
	t.Helper()
	d, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp(\"\", %q-*) failed: %v", prefix, err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// mustNewLogger creates a Logger or fails the test immediately.
// Use this in tests to avoid repeating the error-check boilerplate.
//
// Usage:
//
//	logger := mustNewLogger(t, dir, &config.SystemSettings{})
//	defer logger.Close()
func mustNewLogger(t *testing.T, dir string, cfg *config.SystemSettings) *Logger {
	t.Helper()
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
