package client

import (
	"os"
	"sync/atomic"
)

// cgroupSandboxDisabled controls whether ApplySandbox skips cgroup-based
// resource limits. Toggled by SetCgroupSandboxDisabled at startup and on
// config hot-reload. Replaces the former os.Getenv("ORCH_DISABLE_CGROUP_SANDBOX")
// read in sandbox_other.go for hot-reload support.
//
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md.
var cgroupSandboxDisabled atomic.Bool

func init() {
	if os.Getenv("ORCH_DISABLE_CGROUP_SANDBOX") == "1" {
		cgroupSandboxDisabled.Store(true)
	}
}

// SetCgroupSandboxDisabled toggles the cgroup sandbox disable flag at runtime.
// Called at startup and on config hot-reload.
func SetCgroupSandboxDisabled(disabled bool) { cgroupSandboxDisabled.Store(disabled) }
