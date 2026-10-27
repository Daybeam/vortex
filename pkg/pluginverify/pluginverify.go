// Package pluginverify provides script integrity verification and permission
// filtering for plugin scripts (interceptors, hooks, external providers).
// Shared between core/ and providers/ to avoid circular dependencies.
package pluginverify

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
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

// VerifyScriptSignature checks an Ed25519 signature against a pre-configured
// public key. signatureFile is the path to a .sig file alongside the script.
// publicKeyB64 is a base64-encoded Ed25519 public key from config.
//
// If publicKeyB64 is empty, returns nil (backward compat — no signing configured).
// If the signature file does not exist, returns an error (signature required
// when signing is configured).
//
// Plugin integrity design Change 6 (Phase 2, 2026-10-26).
func VerifyScriptSignature(scriptPath, signatureFile, publicKeyB64 string) error {
	if publicKeyB64 == "" {
		return nil // no signing configured — backward compat
	}
	pubKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return fmt.Errorf("plugin: invalid public key encoding: %w", err)
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("plugin: public key size %d != expected %d", len(pubKeyBytes), ed25519.PublicKeySize)
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	scriptData, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("plugin: cannot read script: %w", err)
	}
	sigData, err := os.ReadFile(signatureFile)
	if err != nil {
		return fmt.Errorf("plugin: cannot read signature file: %w", err)
	}
	if len(sigData) != ed25519.SignatureSize {
		return fmt.Errorf("plugin: signature size %d != expected %d", len(sigData), ed25519.SignatureSize)
	}

	if !ed25519.Verify(pubKey, scriptData, sigData) {
		return fmt.Errorf("plugin: signature verification FAILED for %s", scriptPath)
	}
	return nil
}
