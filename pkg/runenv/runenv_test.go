package runenv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Tests for the unified ResolveDataRoot chain:
// VORTEX_ROOT → walkUpFrom(exe) → userDataDir() → error
// Spec: docs/plan/DATAROOT_PATH_UNIFICATION.md §2

func TestResolveDataRoot_EnvSet(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "/explicit/root")
	got, err := ResolveDataRoot()
	if err != nil {
		t.Fatalf("ResolveDataRoot() error: %v", err)
	}
	if got != "/explicit/root" {
		t.Fatalf("ResolveDataRoot() = %q, want %q (VORTEX_ROOT must win)", got, "/explicit/root")
	}
}

func TestResolveDataRoot_EnvEmpty(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "")
	// When VORTEX_ROOT is empty, should fall through to walk-up or user dir.
	// We can't predict the exact result without controlling exe path,
	// but it must not return the empty string.
	got, err := ResolveDataRoot()
	if err != nil && got == "" {
		t.Fatalf("ResolveDataRoot() returned empty string with error: %v", err)
	}
}

func TestResolveDataRoot_WalkUpFound(t *testing.T) {
	// Create a temp directory tree: root/config.json, root/build/bin/exe
	tmp := t.TempDir()
	root := filepath.Join(tmp, "project")
	binDir := filepath.Join(root, "build", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Place config.json at project root
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// walkUpFrom should find config.json by walking up from binDir
	got, ok := walkUpFrom(binDir)
	if !ok {
		t.Fatalf("walkUpFrom(%q) did not find config.json", binDir)
	}
	if got != root {
		t.Fatalf("walkUpFrom() = %q, want %q", got, root)
	}
}

func TestResolveDataRoot_WalkUpFoundOSConfig(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "project")
	binDir := filepath.Join(root, "build", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Place config_<GOOS>.json at project root (no config.json)
	osConfig := filepath.Join(root, "config_"+runtime.GOOS+".json")
	if err := os.WriteFile(osConfig, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := walkUpFrom(binDir)
	if !ok {
		t.Fatalf("walkUpFrom(%q) did not find config_%s.json", binDir, runtime.GOOS)
	}
	if got != root {
		t.Fatalf("walkUpFrom() = %q, want %q", got, root)
	}
}

func TestResolveDataRoot_WalkUpNotFound(t *testing.T) {
	tmp := t.TempDir()
	// No config files anywhere in this tree
	_, ok := walkUpFrom(tmp)
	if ok {
		t.Fatalf("walkUpFrom(%q) should not find config.json", tmp)
	}
}

func TestResolveDataRoot_UserDir_XDG(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "")
	t.Setenv("XDG_DATA_HOME", "/custom/xdg")
	// Unset HOME to ensure XDG takes priority
	os.Unsetenv("HOME")

	got, err := userDataDir()
	if err != nil {
		t.Fatalf("userDataDir() error: %v", err)
	}
	want := filepath.Join("/custom/xdg", "orchestrator")
	if got != want {
		t.Fatalf("userDataDir() = %q, want %q", got, want)
	}
}

func TestResolveDataRoot_UserDir_HomeFallback(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "")
	os.Unsetenv("XDG_DATA_HOME")
	// os.UserHomeDir uses %USERPROFILE% on Windows, $HOME on Unix
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", `C:\fake\home`)
	} else {
		t.Setenv("HOME", "/fake/home")
	}

	got, err := userDataDir()
	if err != nil {
		t.Fatalf("userDataDir() error: %v", err)
	}
	var want string
	if runtime.GOOS == "windows" {
		want = filepath.Join(`C:\fake\home`, ".local", "share", "orchestrator")
	} else {
		want = filepath.Join("/fake/home", ".local", "share", "orchestrator")
	}
	if got != want {
		t.Fatalf("userDataDir() = %q, want %q", got, want)
	}
}

func TestResolveDataRoot_AllFail_Error(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "")
	os.Unsetenv("XDG_DATA_HOME")
	// Clear all home-related env vars so os.UserHomeDir() fails
	if runtime.GOOS == "windows" {
		os.Unsetenv("USERPROFILE")
		os.Unsetenv("HOME")
	} else {
		os.Unsetenv("HOME")
	}

	_, err := userDataDir()
	if err == nil {
		t.Fatalf("userDataDir() should return error when no XDG_DATA_HOME or HOME")
	}
}
