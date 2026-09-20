package core

import (
	"fmt"
	"os"

	"github.com/daybeam/vortex/config"
)

// SelectSandboxProvider initializes the appropriate sandbox backend based on config (ADDED 2026-09-02).
func SelectSandboxProvider(reg *config.Registry, outputBase string) SandboxProvider {
	reg.Mu.RLock()
	mode := reg.System.Sandbox.Mode
	reg.Mu.RUnlock()

	if outputBase == "" {
		outputBase = "outputs"
	}

	switch mode {
	case "local":
		return NewLocalProvider(outputBase)
	case "bubblewrap":
		return NewBubblewrapProvider(outputBase)
	case "docker":
		// DockerProvider not implemented yet, fallback to local with warning
		fmt.Fprintf(os.Stderr, "[sandbox] WARNING: DockerProvider not implemented, falling back to local\n")
		return NewLocalProvider(outputBase)
	case "remote":
		// RemoteProvider not implemented yet, fallback to local with warning
		fmt.Fprintf(os.Stderr, "[sandbox] WARNING: RemoteProvider not implemented, falling back to local\n")
		return NewLocalProvider(outputBase)
	default:
		return NewLocalProvider(outputBase)
	}
}
