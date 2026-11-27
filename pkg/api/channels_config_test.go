package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// assertPerm checks the file mode on POSIX platforms. Windows does not honor
// POSIX permission bits (files are created 0666 regardless of Chmod), so the
// check is skipped there — the 0600 intent is documented in writeEnvKey.
func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != want {
		t.Fatalf("perm = %v, want %v", perm, want)
	}
}

func TestWriteEnvKey_AppendsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FOO=bar\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeEnvKey(path, "TELEGRAM_BOT_TOKEN", "123:abc"); err != nil {
		t.Fatalf("writeEnvKey: %v", err)
	}

	data, _ := os.ReadFile(path)
	want := "FOO=bar\nTELEGRAM_BOT_TOKEN=123:abc\n"
	if string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
}

func TestWriteEnvKey_ReplacesExistingAndDropsDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	original := "# comment\nTELEGRAM_BOT_TOKEN=old:token\nFOO=bar\nTELEGRAM_BOT_TOKEN=stale:dup\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeEnvKey(path, "TELEGRAM_BOT_TOKEN", "new:token"); err != nil {
		t.Fatalf("writeEnvKey: %v", err)
	}

	data, _ := os.ReadFile(path)
	want := "# comment\nTELEGRAM_BOT_TOKEN=new:token\nFOO=bar\n"
	if string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
	// The value must survive loadDotEnvWithMode parsing (same package family):
	// single KEY=VALUE line, no stray quotes.
	if strings.Count(string(data), "TELEGRAM_BOT_TOKEN=") != 1 {
		t.Fatalf("expected exactly one TELEGRAM_BOT_TOKEN line, got %q", data)
	}
}

func TestWriteEnvKey_CreatesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if err := writeEnvKey(path, "TELEGRAM_BOT_TOKEN", "123:abc"); err != nil {
		t.Fatalf("writeEnvKey: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "TELEGRAM_BOT_TOKEN=123:abc\n" {
		t.Fatalf("got %q", data)
	}
	assertPerm(t, path, 0600)
}

func TestWriteEnvKey_PreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FOO=bar\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeEnvKey(path, "TELEGRAM_BOT_TOKEN", "123:abc"); err != nil {
		t.Fatalf("writeEnvKey: %v", err)
	}

	data, _ := os.ReadFile(path)
	want := "FOO=bar\nTELEGRAM_BOT_TOKEN=123:abc\n"
	if string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
	assertPerm(t, path, 0644)
}

func TestWriteEnvKey_NoTrailingNewlineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	// A file without a trailing newline must not glue lines together.
	if err := os.WriteFile(path, []byte("FOO=bar"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeEnvKey(path, "TELEGRAM_BOT_TOKEN", "123:abc"); err != nil {
		t.Fatalf("writeEnvKey: %v", err)
	}

	data, _ := os.ReadFile(path)
	want := "FOO=bar\nTELEGRAM_BOT_TOKEN=123:abc\n"
	if string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
}

func TestEnvFilePath_EndsWithDotEnv(t *testing.T) {
	got := envFilePath()
	if filepath.Base(got) != ".env" {
		t.Fatalf("envFilePath() = %q, want a path ending in .env", got)
	}
}
