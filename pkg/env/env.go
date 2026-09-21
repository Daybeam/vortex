package env

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	shellVersion     string
	shellOnce        sync.Once
	pythonCmd        string
	pythonOnce       sync.Once
	detectedRuntimes []string
	runtimesOnce     sync.Once
)

// GetShellVersion returns a descriptive string of the current OS default shell version.
// It caches the result after the first call.
func GetShellVersion() string {
	shellOnce.Do(func() {
		shellVersion = detectShellVersion()
	})
	return shellVersion
}

// GetPythonCmd returns the valid python command for the current environment.
// It prioritizes "python3" on Unix and "python" on Windows.
func GetPythonCmd() string {
	pythonOnce.Do(func() {
		primary := "python3"
		secondary := "python"

		if runtime.GOOS == "windows" {
			primary = "python"
			secondary = "python3"
		}

		if _, err := exec.LookPath(primary); err == nil {
			pythonCmd = primary
			return
		}
		if _, err := exec.LookPath(secondary); err == nil {
			pythonCmd = secondary
			return
		}
		pythonCmd = primary
	})
	return pythonCmd
}

// DetectRuntimes probes the host for available runtimes and returns a list.
// The result is cached after the first call.
func DetectRuntimes() []string {
	runtimesOnce.Do(func() {
		probes := map[string]string{
			"python": GetPythonCmd(),
			"node":   "node",
			"docker": "docker",
			"git":    "git",
		}

		for label, cmd := range probes {
			if _, err := exec.LookPath(cmd); err == nil {
				detectedRuntimes = append(detectedRuntimes, label)
			}
		}
	})
	return detectedRuntimes
}

func detectShellVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	shellName := ""
	if runtime.GOOS == "windows" {
		shellName = "PowerShell"
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", "$PSVersionTable.PSVersion.ToString()")
	} else {
		shellName = "Bash"
		cmd = exec.CommandContext(ctx, "bash", "--version")
	}

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "unknown"
	}

	line, _, _ := bufio.NewReader(&out).ReadLine()
	version := strings.TrimSpace(string(line))
	if runtime.GOOS != "windows" {
		// Bash version line is long, try to truncate it to something reasonable
		if parts := strings.Split(version, " "); len(parts) > 3 {
			version = parts[3]
		}
	}

	return fmt.Sprintf("%s %s", shellName, version)
}
