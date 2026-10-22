package sandbox

import (
	"fmt"
	"os"
)

func NewBubblewrapProvider(outputBase string) SandboxProvider {
	fmt.Fprintf(os.Stderr, "[sandbox] WARNING: bubblewrap not available on this platform, falling back to local\n")
	return NewLocalProvider(outputBase)
}

func NewDockerProvider(outputBase, image, network string) SandboxProvider {
	fmt.Fprintf(os.Stderr, "[sandbox] WARNING: docker sandbox not available on this platform, falling back to local\n")
	return NewLocalProvider(outputBase)
}

func BuildBwrapArgs(command string, args []string, workDir string) (bwrapPath string, bwrapArgs []string) {
	return "", nil
}
