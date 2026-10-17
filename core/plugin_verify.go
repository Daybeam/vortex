package core

import (
	"github.com/daybeam/vortex/pkg/pluginverify"
)

// VerifyScriptIntegrity checks the script file's SHA256 against the expected hash.
// Delegates to pkg/pluginverify (shared with providers/ to avoid circular deps).
func VerifyScriptIntegrity(scriptPath, expectedHash string) error {
	return pluginverify.VerifyScriptIntegrity(scriptPath, expectedHash)
}

// FilterEnvVars returns only the environment variables whose names are in the
// allowed list. Delegates to pkg/pluginverify.
func FilterEnvVars(fullEnv []string, allowed []string) []string {
	return pluginverify.FilterEnvVars(fullEnv, allowed)
}

// IsWriteTool returns true if the tool name suggests a write/mutate operation.
// Delegates to pkg/pluginverify.
func IsWriteTool(toolName string) bool {
	return pluginverify.IsWriteTool(toolName)
}
