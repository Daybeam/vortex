//go:build windows

package client

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
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

// --- Windows Job Object plumbing (stdlib syscall only, no extra module deps) ---

const (
	jobObjectExtendedLimitInformation = 9

	jobObjectLimitProcessTime    = 0x00000002
	jobObjectLimitProcessMemory  = 0x00000100
	jobObjectLimitKillOnJobClose = 0x00002000

	processTerminate = 0x0001
	processSetQuota  = 0x0100
)

// Layouts below mirror the Win32 structs field-for-field (same field order and
// widths as JOBOBJECT_BASIC_LIMIT_INFORMATION / IO_COUNTERS /
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION). Go's default struct alignment on
// amd64 matches the C compiler's here (uintptr fields naturally pad to 8-byte
// boundaries), which is the same assumption golang.org/x/sys/windows relies
// on for the same structs -- this local copy avoids adding a new module
// dependency for four API calls.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformationT struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	modkernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW      = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObj  = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj = modkernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess           = modkernel32.NewProc("OpenProcess")
	procCloseHandle           = modkernel32.NewProc("CloseHandle")
)

// ApplySandbox creates a Windows Job Object with a process-memory cap and a
// per-process CPU-time cap, assigns the target pid to it, and returns a
// cleanup closure that releases the job/process handles.
//
// If c.KillOnClose is set, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE is applied:
// Windows terminates any process still assigned to the job the moment the
// last handle to the job object is closed (including implicitly, if this Go
// process exits without calling cleanup) -- this is what actually prevents a
// runaway or orphaned JIT-spawned subprocess from surviving its sandbox.
//
// This intentionally only ever gets called for MCPDef.Sandboxed == true
// bindings (see client.NewMCPClient's `sandboxed` parameter and its caller in
// core/spawner.go), i.e. JIT-registered tools -- never for MCPs Connor
// configured directly, so the limits below are not applied to trusted,
// resource-heavy servers like gitnexus or chrome-devtools.
func ApplySandbox(pid int, c SandboxConstraints) (cleanup func(), err error) {
	noop := func() {}

	hJob, _, e := procCreateJobObjectW.Call(0, 0)
	if hJob == 0 {
		return noop, fmt.Errorf("jit-sandbox: CreateJobObjectW failed: %v", e)
	}

	var limitFlags uint32 = jobObjectLimitProcessMemory
	if c.KillOnClose {
		limitFlags |= jobObjectLimitKillOnJobClose
	}
	var perProcessUserTime int64
	if c.MaxCPUSeconds > 0 {
		limitFlags |= jobObjectLimitProcessTime
		perProcessUserTime = int64(c.MaxCPUSeconds) * 10_000_000 // 100ns units
	}

	info := jobObjectExtendedLimitInformationT{
		BasicLimitInformation: jobObjectBasicLimitInformation{
			LimitFlags:              limitFlags,
			PerProcessUserTimeLimit: perProcessUserTime,
		},
		ProcessMemoryLimit: uintptr(c.MaxMemoryBytes),
	}

	ret, _, e := procSetInformationJobObj.Call(
		hJob,
		uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ret == 0 {
		procCloseHandle.Call(hJob)
		return noop, fmt.Errorf("jit-sandbox: SetInformationJobObject failed: %v", e)
	}

	hProcess, _, e := procOpenProcess.Call(
		uintptr(processTerminate|processSetQuota),
		0,
		uintptr(pid),
	)
	if hProcess == 0 {
		procCloseHandle.Call(hJob)
		return noop, fmt.Errorf("jit-sandbox: OpenProcess failed for pid %d: %v", pid, e)
	}

	ret, _, e = procAssignProcessToJobObj.Call(hJob, hProcess)
	if ret == 0 {
		procCloseHandle.Call(hProcess)
		procCloseHandle.Call(hJob)
		return noop, fmt.Errorf("jit-sandbox: AssignProcessToJobObject failed for pid %d: %v", pid, e)
	}

	fmt.Fprintf(os.Stderr,
		"[jit-sandbox] pid=%d maxMem=%dMB maxCPU=%ds killOnClose=%v -- Job Object enforced\n",
		pid, c.MaxMemoryBytes/(1024*1024), c.MaxCPUSeconds, c.KillOnClose)

	cleanup = func() {
		procCloseHandle.Call(hProcess)
		procCloseHandle.Call(hJob)
	}
	return cleanup, nil
}
