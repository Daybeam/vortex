// Package pluginverify provides script integrity verification and permission
// filtering for plugin scripts (interceptors, hooks, external providers).
// Shared between core/ and providers/ to avoid circular dependencies.
package pluginverify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// VerifyScriptIntegrity checks the script file's SHA256 against the expected hash.
// If expectedHash is empty, returns nil (backward compat — caller should log a warning).
// If non-empty and mismatched, returns an error — the script is rejected.
func VerifyScriptIntegrity(scriptPath, expectedHash string) error {
	if expectedHash == "" {
		return nil
	}
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("plugin: cannot read script %s: %w", scriptPath, err)
	}
	actual := sha256.Sum256(data)
	actualHex := hex.EncodeToString(actual[:])
	if actualHex != expectedHash {
		return fmt.Errorf(
			"plugin: integrity check FAILED for %s\n  expected: %s\n  actual:   %s\nThe script file has been modified or tampered with.",
			scriptPath, expectedHash, actualHex,
		)
	}
	return nil
}

// FilterEnvVars returns only the environment variables whose names are in the
// allowed list. If allowed is empty, returns nil (no env vars = empty environment).
func FilterEnvVars(fullEnv []string, allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = true
	}
	var result []string
	for _, kv := range fullEnv {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		if allowedSet[key] {
			result = append(result, kv)
		}
	}
	return result
}

// writeToolPrefixes is the set of verb prefixes that indicate a write/mutate
// operation. Hoisted to package level (audit P-17-2) so IsWriteTool does not
// re-allocate the slice on every call. See AUDIT_REPORT_2026-10-17.md §P-17-2.
var writeToolPrefixes = []string{
	"create", "update", "delete", "execute", "set", "put", "post",
	"write", "insert", "remove", "drop", "alter", "edit", "modify",
	"grant", "revoke", "deploy", "install", "uninstall", "start", "stop",
	"restart", "kill", "terminate", "submit", "commit", "push", "send",
}

// IsWriteTool returns true if the tool name suggests a write/mutate operation.
// Used to filter tools for untrusted MCPs.
func IsWriteTool(toolName string) bool {
	name := strings.ToLower(toolName)
	for _, prefix := range writeToolPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
		if strings.Contains(name, "_"+prefix+"_") || strings.HasSuffix(name, "_"+prefix) {
			return true
		}
	}
	return false
}
