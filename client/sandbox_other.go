//go:build !windows && !darwin

package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type SandboxConstraints struct {
	MaxMemoryBytes uint64
	MaxCPUSeconds  uint64
	KillOnClose    bool
}

var DefaultSandboxConstraints = SandboxConstraints{
	MaxMemoryBytes: 256 * 1024 * 1024, // 256 MB
	MaxCPUSeconds:  30,
	KillOnClose:    true,
}

// cgroupRoot is the cgroups v2 mount point.
var cgroupRoot string

func init() {
	for _, root := range []string{
		"/sys/fs/cgroup",
		"/sys/fs/cgroup/system.slice",
	} {
		if _, err := os.Stat(root); err == nil {
			cgroupRoot = root
			break
		}
	}
}

// ApplySandbox enforces resource limits on the given process using cgroups v2.
//
// Memory:    written to memory.max (hard limit — OOM killer on breach).
// CPU:       a watchdog goroutine sends SIGKILL after MaxCPUSeconds.
// KillOnClose: when cleanup() is called, the cgroup directory is removed,
//
//	which terminates any remaining processes — mirroring Windows Job Object.
func ApplySandbox(pid int, c SandboxConstraints) (cleanup func(), err error) {
	if cgroupRoot == "" {
		return func() {}, fmt.Errorf("jit-sandbox: cgroups v2 mount not found")
	}

	cgroupDir := filepath.Join(cgroupRoot,
		"orchestrator-jit-"+strconv.Itoa(pid)+"-"+strconv.FormatUint(uint64(time.Now().UnixNano()), 16))

	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		return func() {}, fmt.Errorf("jit-sandbox: mkdir %s: %w", cgroupDir, err)
	}

	// ---- Memory limit ----
	if c.MaxMemoryBytes > 0 {
		if err := os.WriteFile(
			filepath.Join(cgroupDir, "memory.max"),
			[]byte(strconv.FormatUint(c.MaxMemoryBytes, 10)),
			0644,
		); err != nil {
			return func() { os.RemoveAll(cgroupDir) },
				fmt.Errorf("jit-sandbox: set memory.max for pid=%d: %w", pid, err)
		}
	}

	// ---- Assign PID to cgroup ----
	if err := os.WriteFile(
		filepath.Join(cgroupDir, "cgroup.procs"),
		[]byte(strconv.Itoa(pid)),
		0644,
	); err != nil {
		return func() { os.RemoveAll(cgroupDir) },
			fmt.Errorf("jit-sandbox: assign pid=%d to cgroup: %w", pid, err)
	}

	// ---- CPU watchdog: fire-and-forget, no sync needed ----
	var cpuTimer *time.Timer
	if c.MaxCPUSeconds > 0 {
		cpuTimer = time.AfterFunc(time.Duration(c.MaxCPUSeconds)*time.Second, func() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		})
	}

	fmt.Fprintf(os.Stderr,
		"[jit-sandbox] pid=%d maxMem=%dMB maxCPU=%ds killOnClose=%v -- cgroup %s\n",
		pid, c.MaxMemoryBytes/(1024*1024), c.MaxCPUSeconds, c.KillOnClose, cgroupDir)

	cleanup = func() {
		if cpuTimer != nil {
			cpuTimer.Stop()
		}
		os.RemoveAll(cgroupDir)
	}

	return cleanup, nil
}
