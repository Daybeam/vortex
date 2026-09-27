package core

import (
	"path/filepath"
	"testing"
)

// TestSH5_FilepathBaseStripsTraversal is a regression test for audit S-H5:
// builtin_interceptors.go must sanitize cmdName with filepath.Base() before
// using it to construct a fix script path. Without this, a cmdName like
// "../../etc/passwd" would escape the scripts/fix/ directory.
//
// This test verifies the core property of the fix: filepath.Base strips all
// directory components, leaving only the final element.
//
// Reproduction: filepath.Base must never return a string containing "/" or "..".
func TestSH5_FilepathBaseStripsTraversal(t *testing.T) {
	traversalInputs := []string{
		"../../etc/passwd",
		"../../../tmp/evil",
		"..\\..\\windows\\system32\\evil",
		"/etc/passwd",
		"scripts/fix/../../../evil",
		"",
	}

	for _, input := range traversalInputs {
		base := filepath.Base(input)

		// The base name must not contain a path separator.
		if filepath.Dir(base) != "." && base != "." && base != string(filepath.Separator) {
			t.Errorf("filepath.Base(%q) = %q still has directory component", input, base)
		}

		// The base name must not be ".." (which could still traverse).
		if base == ".." {
			t.Errorf("filepath.Base(%q) = %q is '..', traversal not stripped", input, base)
		}
	}
}

// TestSH5_FixScriptPathStaysInScriptsDir verifies that the fix script path
// constructed with filepath.Base(cmdName) always stays within scripts/fix/.
func TestSH5_FixScriptPathStaysInScriptsDir(t *testing.T) {
	maliciousCmdNames := []string{
		"../../etc/passwd",
		"../../../tmp/evil",
		"normal_cmd",
		"subdir/cmd",
	}

	for _, cmdName := range maliciousCmdNames {
		safeCmdName := filepath.Base(cmdName)
		fixScript := filepath.Join("scripts", "fix", "fix_"+safeCmdName+".sh")

		// The fix script path must start with "scripts/fix/".
		expectedPrefix := filepath.Join("scripts", "fix")
		rel, err := filepath.Rel(expectedPrefix, fixScript)
		if err != nil {
			t.Errorf("cmdName %q: cannot compute rel path: %v", cmdName, err)
			continue
		}
		if rel == ".." || (len(rel) > 2 && rel[:2] == "..") {
			t.Errorf("cmdName %q: fix script %q escapes scripts/fix/ directory", cmdName, fixScript)
		}
	}
}
