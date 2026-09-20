package core

import (
	"github.com/daybeam/vortex/pkg/env"
	"testing"
)

func TestDetectRuntimes(t *testing.T) {
	runtimes := env.DetectRuntimes()
	t.Logf("Detected runtimes: %v", runtimes)

	// In this environment, we know python is available (verified by adb shell earlier)
	foundPython := false
	for _, r := range runtimes {
		if r == "python" {
			foundPython = true
			break
		}
	}
	if !foundPython {
		t.Error("expected python to be detected in this environment")
	}
}

func TestGetPythonCmd(t *testing.T) {
	cmd := getPythonCmd()
	if cmd == "" {
		t.Fatal("expected non-empty python command")
	}
	t.Logf("Python command: %s", cmd)
}
