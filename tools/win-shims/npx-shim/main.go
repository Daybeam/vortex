package main

import (
	"os"
	"os/exec"
)

// npx.exe shim: forwards all args to npx.cmd via an explicit extension.
// This exists purely to work around a Windows-specific Node.js/libuv bug:
// child_process.spawn('npx', args) without {shell: true} fails with ENOENT
// because libuv's extensionless-command PATHEXT search does not reliably
// resolve to npx.cmd on this machine/Node version. Node DOES correctly
// handle explicit .cmd targets (auto-wraps with cmd.exe internally), so
// re-invoking with the .cmd extension spelled out here fixes it.
//
// Placed as npx.exe in a directory that is searched (via PATHEXT, .EXE
// before .CMD) before npx.cmd is found, any extensionless spawn('npx', ...)
// call -- including the ones inside @mizchi/lsmcp that orchestrator's
// pyright MCP binding depends on -- will hit this shim first and succeed.
func main() {
	cmd := exec.Command("npx.cmd", os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
	if cmd.ProcessState != nil {
		os.Exit(cmd.ProcessState.ExitCode())
	}
	os.Exit(1)
}
