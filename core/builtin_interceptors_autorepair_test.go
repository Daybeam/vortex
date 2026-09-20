package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestHealthCheckInterceptor_EnableAutoRepair_Gating verifies the VDA
// self-healing gate documented in the 2026-08-31/2026-09-05/2026-09-06
// addenda (S3, S3.1): a missing local MCP command dependency must NOT
// trigger fix-script execution unless registry.System.EnableAutoRepair is
// explicitly set to true. This is a safety gate deliberately mirroring the
// F25/F26 "no unattended code execution without an opt-in flag" posture.
//
// Before this test, EnableAutoRepair had zero test coverage despite gating
// a real shell-execution code path -- flagged as a gap in the 09-06
// addendum's priority list.
func TestHealthCheckInterceptor_EnableAutoRepair_Gating(t *testing.T) {
	const fakeCmd = "orchestrator_autorepair_probe_cmd_xyz"

	// Create a fix script at the exact relative path the interceptor looks
	// for (relative to process cwd -- for `go test ./core/...` this is the
	// core/ package directory itself).
	ext := "bat"
	scriptBody := "@echo off\r\n"
	if runtime.GOOS != "windows" {
		ext = "sh"
		scriptBody = "#!/bin/sh\nexit 0\n"
	}
	fixDir := filepath.Join("scripts", "fix")
	if err := os.MkdirAll(fixDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", fixDir, err)
	}
	fixScript := filepath.Join(fixDir, "fix_"+fakeCmd+"."+ext)
	if err := os.WriteFile(fixScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", fixScript, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(fixScript)
		_ = os.Remove(fixDir)
		_ = os.Remove(filepath.Join("scripts"))
	})

	newRegistry := func(enableAutoRepair bool) *config.Registry {
		reg := &config.Registry{
			System: config.SystemSettings{EnableAutoRepair: enableAutoRepair},
		}
		reg.Roles = map[string]*config.Role{
			"probe_role": {
				ID: "probe_role",
				BoundMCPBindings: []config.MCPBinding{
					{MCPID: "probe_mcp"},
				},
			},
		}
		reg.MCPs = map[string]*config.MCPDef{
			"probe_mcp": {ID: "probe_mcp", Command: fakeCmd},
		}
		return reg
	}

	noopNext := func(ctx context.Context, r *SpawnRequest) (*SpawnResult, error) {
		return &SpawnResult{}, nil
	}

	t.Run("disabled (default): no repair attempt logged, MCP is degraded", func(t *testing.T) {
		logger, err := NewLogger(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}

		reg := newRegistry(false)
		interceptor := HealthCheckInterceptor(reg, logger)
		req := &SpawnRequest{TaskID: "task_autorepair_off", StepID: "s1", RoleID: "probe_role"}

		if _, err := interceptor(context.Background(), req, noopNext); err != nil {
			t.Fatalf("interceptor returned error: %v", err)
		}

		if len(req.DegradedMCPs) != 1 || req.DegradedMCPs[0] != "probe_mcp" {
			t.Fatalf("expected probe_mcp to be degraded, got %v", req.DegradedMCPs)
		}

		logger.Close()
		events, err := logger.ReadTaskLogs(req.TaskID)
		if err != nil {
			t.Fatalf("ReadTaskLogs: %v", err)
		}
		for _, ev := range events {
			if ev["event"] == "EventEnvironmentRepairStarted" {
				t.Fatalf("EnableAutoRepair=false must never log EventEnvironmentRepairStarted, but it did: %v", ev)
			}
		}
	})

	t.Run("enabled: repair attempt is logged when a fix script exists", func(t *testing.T) {
		logger, err := NewLogger(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}

		reg := newRegistry(true)
		interceptor := HealthCheckInterceptor(reg, logger)
		req := &SpawnRequest{TaskID: "task_autorepair_on", StepID: "s1", RoleID: "probe_role"}

		if _, err := interceptor(context.Background(), req, noopNext); err != nil {
			t.Fatalf("interceptor returned error: %v", err)
		}

		// The fix script is a no-op stub that doesn't actually install
		// fakeCmd, so the dependency remains missing and the MCP should
		// still end up degraded after the (attempted, failed-to-fully-fix)
		// repair -- this test only asserts that the attempt itself was
		// made and logged when the gate is open, not that repair magically
		// succeeds.
		if len(req.DegradedMCPs) != 1 || req.DegradedMCPs[0] != "probe_mcp" {
			t.Fatalf("expected probe_mcp to still be degraded (fix script is a no-op stub), got %v", req.DegradedMCPs)
		}

		logger.Close()
		events, err := logger.ReadTaskLogs(req.TaskID)
		if err != nil {
			t.Fatalf("ReadTaskLogs: %v", err)
		}
		found := false
		for _, ev := range events {
			if ev["event"] == "EventEnvironmentRepairStarted" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("expected EventEnvironmentRepairStarted to be logged when EnableAutoRepair=true and a fix script exists")
		}
	})

	t.Run("enabled but no fix script present: behaves exactly like disabled", func(t *testing.T) {
		// Remove the script for this subtest only, restore afterward so
		// later subtests (if any were added) aren't affected.
		_ = os.Remove(fixScript)
		defer func() {
			_ = os.WriteFile(fixScript, []byte(scriptBody), 0o755)
		}()

		logger, err := NewLogger(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}

		reg := newRegistry(true)
		interceptor := HealthCheckInterceptor(reg, logger)
		req := &SpawnRequest{TaskID: "task_autorepair_on_noscript", StepID: "s1", RoleID: "probe_role"}

		if _, err := interceptor(context.Background(), req, noopNext); err != nil {
			t.Fatalf("interceptor returned error: %v", err)
		}

		logger.Close()
		events, err := logger.ReadTaskLogs(req.TaskID)
		if err != nil {
			t.Fatalf("ReadTaskLogs: %v", err)
		}
		for _, ev := range events {
			if ev["event"] == "EventEnvironmentRepairStarted" {
				t.Fatalf("no fix script exists on disk, so no repair attempt should have been logged: %v", ev)
			}
		}
	})
}
