package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanSandboxRunDir_RemovesOldKeepsNew(t *testing.T) {
	// Use a temp working dir so the test doesn't touch real outputs/.
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	base := sandboxRunDir
	os.MkdirAll(base, 0755)

	// Create an "old" dir (25h ago) and a "new" dir (1h ago).
	oldDir := filepath.Join(base, "old_run")
	newDir := filepath.Join(base, "new_run")
	os.MkdirAll(oldDir, 0755)
	os.MkdirAll(newDir, 0755)

	// Set mod times: old dir is 25h old, new dir is 1h old.
	oldTime := time.Now().Add(-25 * time.Hour)
	newTime := time.Now().Add(-1 * time.Hour)
	os.Chtimes(oldDir, oldTime, oldTime)
	os.Chtimes(newDir, newTime, newTime)

	cleanSandboxRunDir()

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("expected old dir %s to be removed, but it exists", oldDir)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("expected new dir %s to survive, but it was removed: %v", newDir, err)
	}
}

func TestCleanSandboxRunDir_EmptyOrMissingDir(t *testing.T) {
	// Should not panic when dir doesn't exist.
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Don't create the dir — cleanSandboxRunDir should handle gracefully.
	cleanSandboxRunDir() // must not panic
}
