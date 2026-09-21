package core

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SafePathValidator provides three-layer path validation for file operations:
// Level 1: global allowed_workspaces (config.json)
// Level 2: session-level workspace root (runtime binding)
// Level 3: task-level output directory (outputs/{taskID}/)
//
// When SessionRoot and AllowedRoots are empty, behavior is identical to
// the legacy safeJoin (backward compatible). See
// docs/architecture/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2.
type SafePathValidator struct {
	TaskDir      string
	SessionRoot  string
	AllowedRoots []string
}

// NewSafePathValidator creates a validator from the given parameters.
// When sessionRoot and allowedRoots are empty, Resolve falls back to
// taskDir-only containment (same as legacy safeJoin).
func NewSafePathValidator(taskDir, sessionRoot string, allowedRoots []string) *SafePathValidator {
	return &SafePathValidator{
		TaskDir:      taskDir,
		SessionRoot:  sessionRoot,
		AllowedRoots: allowedRoots,
	}
}

// Resolve validates a relative path against the allowed roots and returns
// the absolute path. The base parameter selects which root to resolve
// against: "task" → TaskDir, "session" → SessionRoot, or a key in
// AllowedRoots. When base is empty, defaults to "task".
//
// rel must be a relative path (no ".." traversal, no absolute paths).
func (v *SafePathValidator) Resolve(base, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, string(filepath.Separator)) || strings.Contains(rel, ":") {
		return "", fmt.Errorf("path traversal detected: %s", rel)
	}

	root := v.selectRoot(base)
	if root == "" {
		return "", fmt.Errorf("no allowed root for base %q", base)
	}

	full := filepath.Join(root, cleaned)
	absRoot, _ := filepath.Abs(root)
	absFull, _ := filepath.Abs(full)
	if !strings.HasPrefix(absFull, absRoot) {
		return "", fmt.Errorf("path escapes allowed directory: %s", rel)
	}
	return full, nil
}

// selectRoot returns the base directory for the given base key.
func (v *SafePathValidator) selectRoot(base string) string {
	switch base {
	case "", "task":
		return v.TaskDir
	case "session":
		return v.SessionRoot
	default:
		for _, r := range v.AllowedRoots {
			if r == base {
				return r
			}
		}
	}
	return ""
}

// IsAllowed checks whether a given absolute path is contained within any
// of the allowed roots (TaskDir, SessionRoot, or AllowedRoots).
func (v *SafePathValidator) IsAllowed(absPath string) bool {
	candidates := []string{v.TaskDir, v.SessionRoot}
	candidates = append(candidates, v.AllowedRoots...)
	absTarget, _ := filepath.Abs(absPath)
	for _, root := range candidates {
		if root == "" {
			continue
		}
		absRoot, _ := filepath.Abs(root)
		if strings.HasPrefix(absTarget, absRoot) {
			return true
		}
	}
	return false
}
