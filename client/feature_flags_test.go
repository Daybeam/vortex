package client

import "testing"

// TestSetCgroupSandboxDisabled verifies the atomic flag can be toggled.
func TestSetCgroupSandboxDisabled(t *testing.T) {
	// Save and restore
	defer SetCgroupSandboxDisabled(false)

	SetCgroupSandboxDisabled(true)
	if !cgroupSandboxDisabled.Load() {
		t.Error("cgroupSandboxDisabled should be true after SetCgroupSandboxDisabled(true)")
	}

	SetCgroupSandboxDisabled(false)
	if cgroupSandboxDisabled.Load() {
		t.Error("cgroupSandboxDisabled should be false after SetCgroupSandboxDisabled(false)")
	}
}
