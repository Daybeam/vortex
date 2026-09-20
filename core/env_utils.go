package core

import "github.com/daybeam/vortex/pkg/env"

// getPythonCmd returns the valid python command for the current environment.
// Wrapper for pkg/env.GetPythonCmd to keep core internal callers unchanged.
func getPythonCmd() string {
	return env.GetPythonCmd()
}
