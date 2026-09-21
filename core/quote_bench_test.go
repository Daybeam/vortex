//go:build test
// +build test

package core

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestQuoteBench_PowerShell_NestedJSON(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test only for Windows")
	}

	exec := NewControlledExecutor()
	exec.DiagnosticMode = true

	// This is a classic QuoteBench failure case.
	// The LLM wants to print a JSON string.
	// Native command: Write-Host '{"foo": "bar"}'
	script := `Write-Host '{"foo": "bar"}'`

	cmd := "powershell"
	// Direct path will use -Command. Script path will use -File.
	args := []string{"-NoProfile", "-NonInteractive", "-Command", script}

	res, err := exec.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	t.Logf("Direct Stdout: %q", string(res.Stdout))
	t.Logf("Direct Stderr: %q", string(res.Stderr))

	if res.Diagnostic != nil {
		t.Logf("TransportDamage: %v", res.Diagnostic.TransportDamage)
		t.Logf("ScriptStdout: %q", string(res.Diagnostic.ScriptStdout))
		t.Logf("FixAttempted: %v", res.Diagnostic.FixAttempted)
	}

	// The expected output is the JSON string
	expected := `{"foo": "bar"}`
	if !strings.Contains(string(res.Stdout), expected) {
		t.Errorf("Result did not contain expected JSON. Got: %q", string(res.Stdout))
	}
}

func TestQuoteBench_PowerShell_SpecialChars(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test only for Windows")
	}

	exec := NewControlledExecutor()
	exec.DiagnosticMode = true

	// Command with multiple special characters that might be mis-parsed on CLI
	script := `Write-Host "Specials: & | ^ % $"`

	cmd := "powershell"
	args := []string{"-NoProfile", "-NonInteractive", "-Command", script}

	res, err := exec.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !strings.Contains(string(res.Stdout), "Specials: & | ^ % $") {
		t.Errorf("Unexpected output: %q", string(res.Stdout))
	}
}

func TestQuoteBench_PowerShell_Multiline(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test only for Windows")
	}

	exec := NewControlledExecutor()
	exec.DiagnosticMode = true

	script := "Write-Host 'Line 1'\nWrite-Host 'Line 2'"

	cmd := "powershell"
	args := []string{"-NoProfile", "-NonInteractive", "-Command", script}

	res, err := exec.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !strings.Contains(string(res.Stdout), "Line 1") || !strings.Contains(string(res.Stdout), "Line 2") {
		t.Errorf("Unexpected output: %q", string(res.Stdout))
	}
}

func TestQuoteBench_CMD_SpecialChars(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("CMD test only for Windows")
	}

	exec := NewControlledExecutor()
	exec.DiagnosticMode = true

	// CMD path: we want to see if & is handled correctly
	script := `echo "A & B"`

	cmd := "cmd"
	args := []string{"/c", script}

	res, err := exec.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	t.Logf("Direct Stdout: %q", string(res.Stdout))

	if res.Diagnostic != nil {
		t.Logf("TransportDamage: %v", res.Diagnostic.TransportDamage)
		t.Logf("ScriptStdout: %q", string(res.Diagnostic.ScriptStdout))
	}

	if !strings.Contains(string(res.Stdout), "A & B") {
		t.Errorf("Result did not contain 'A & B'. Got: %q", string(res.Stdout))
	}
}

func TestQuoteBench_PowerShell_BacktickExpansion(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test only for Windows")
	}

	exec := NewControlledExecutor()
	exec.DiagnosticMode = true

	// Bash-style backticks in PowerShell.
	// The LLM generates: echo `whoami`
	// Direct: powershell -Command "echo `whoami`" -> PowerShell sees ` as escape, might output "echo whoami" literal
	script := "echo `whoami`"

	cmd := "powershell"
	args := []string{"-NoProfile", "-NonInteractive", "-Command", script}

	res, err := exec.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	t.Logf("Direct Stdout: %q", string(res.Stdout))

	if res.Diagnostic != nil {
		t.Logf("TransportDamage: %v", res.Diagnostic.TransportDamage)
		t.Logf("ScriptStdout: %q", string(res.Diagnostic.ScriptStdout))
	}

	// We expect damage here because ` is not expansion in PS
}

func TestQuoteBench_PowerShell_QuoteStripping(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test only for Windows")
	}

	exec := NewControlledExecutor()
	exec.DiagnosticMode = true

	// Command: Write-Host '"Hello"'
	// The LLM wants to print Hello with double quotes.
	script := `Write-Host '"Hello"'`

	cmd := "powershell"
	args := []string{"-NoProfile", "-NonInteractive", "-Command", script}

	res, err := exec.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	t.Logf("Direct Stdout: %q", string(res.Stdout))

	if res.Diagnostic != nil {
		t.Logf("TransportDamage: %v", res.Diagnostic.TransportDamage)
		t.Logf("ScriptStdout: %q", string(res.Diagnostic.ScriptStdout))
		t.Logf("FixAttempted: %v", res.Diagnostic.FixAttempted)
	}

	if res.Diagnostic != nil && res.Diagnostic.TransportDamage {
		t.Log("SUCCESS: Transport damage detected!")
	}
}
