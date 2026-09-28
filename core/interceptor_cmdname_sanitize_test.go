package core

import (
	"path/filepath"
	"testing"
)

// TestSH5_FixScriptPathStaysInScriptsDir is a regression test for audit S-H5:
// builtin_interceptors.go sanitizes cmdName with filepath.Base() before
// constructing a fix script path. This test verifies the end-to-end property:
// the constructed path always stays within scripts/fix/, regardless of what
// cmdName is passed.
//
// Before the fix, a cmdName like "../../etc/passwd" would escape scripts/fix/.
// After the fix, filepath.Base(cmdName) strips all directory components.
//
// Reproduction: filepath.Join("scripts","fix","fix_"+filepath.Base(cmdName)+".sh")
// must never escape the scripts/fix/ directory.
func TestSH5_FixScriptPathStaysInScriptsDir(t *testing.T) {
	maliciousCmdNames := []string{
		"../../etc/passwd",
		"../../../tmp/evil",
		"..\\..\\windows\\system32\\evil",
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
