package sandbox

import (
	"fmt"
	"os"

	"github.com/daybeam/vortex/config"
)

// SelectSandboxProvider initializes the appropriate sandbox backend based on config (ADDED 2026-09-02).
func SelectSandboxProvider(reg *config.Registry, outputBase string) SandboxProvider {
	// audit C-MED-1: hold single RLock for all config reads — previously
	// released between mode read and docker config read, allowing config
	// to change in between (TOCTOU race).
	reg.Mu.RLock()
	mode := reg.System.Sandbox.Mode
	image := reg.System.Sandbox.DockerImage
	network := reg.System.Sandbox.DockerNetwork
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
		return NewDockerProvider(outputBase, image, network)
	case "remote":
		if reg.System.Sandbox.RemoteURL == "" {
			fmt.Fprintf(os.Stderr, "[sandbox] WARNING: remote mode requires sandbox.remote_url, falling back to local\n")
			return NewLocalProvider(outputBase)
		}
		return NewRemoteProvider(reg.System.Sandbox.RemoteURL, reg.System.Sandbox.RemoteToken)
	default:
		return NewLocalProvider(outputBase)
	}
}
