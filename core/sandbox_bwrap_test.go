package core

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildBwrapArgs_NonLinuxReturnsNil(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("test for non-Linux fallback behavior")
	}
	// On Windows/macOS, BuildBwrapArgs must return empty — graceful fallback.
	path, args := BuildBwrapArgs("python", []string{"script.py"}, "/tmp/work")
	if path != "" {
		t.Errorf("expected empty bwrap path on %s, got %q", runtime.GOOS, path)
	}
	if args != nil {
		t.Errorf("expected nil bwrap args on %s, got %v", runtime.GOOS, args)
	}
}

func TestBuildBwrapArgs_LinuxReturnsArgs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("test for Linux bwrap args construction")
	}
	// On Linux, if bwrap is installed, should return path + args.
	path, args := BuildBwrapArgs("python", []string{"script.py"}, "/tmp/work")
	if path == "" {
		t.Skip("bwrap not installed on this Linux host")
	}
	if args == nil {
		t.Fatal("expected non-nil args when bwrap is available")
	}
	// Verify key bwrap flags are present.
	joined := strings.Join(args, " ")
	for _, flag := range []string{"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--proc", "/proc", "--dev", "/dev"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("expected bwrap flag %q in args", flag)
		}
	}
	// Verify the original command arg is appended at the end.
	if args[len(args)-1] != "script.py" {
		t.Errorf("expected original arg 'script.py' at end, got %q", args[len(args)-1])
	}
}
