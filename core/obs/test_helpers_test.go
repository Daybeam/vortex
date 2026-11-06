package obs

import (
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
)

func mustTempDir(t *testing.T, prefix string) string {
	t.Helper()
	d, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp(\"\", %q-*) failed: %v", prefix, err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func mustNewLogger(t *testing.T, dir string, cfg *config.SystemSettings) *Logger {
	t.Helper()
	logger, err := NewLogger(dir, cfg)
	if err != nil {
		t.Fatalf("NewLogger(%q, ...) failed: %v", dir, err)
	}
	t.Cleanup(func() { logger.Close() })
	return logger
}
