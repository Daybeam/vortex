package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Completion checks: the engine decides *when* an artefact counts as done and
// *how a verdict gets back to the model*, but it does not gain the ability to
// judge the artefact itself.
//
// Why not compile the ability in (a SQLite driver for SQL, a schema library for
// JSON, an xlsx reader for spreadsheets): every such addition puts a capability
// inside a 33 MB single binary and makes the next artefact type a code change.
// Why not spawn a command ourselves (`command_pass`): that is the engine acting
// as a shell host, which is exactly what the S-C1 metacharacter ban exists to
// contain.
//
// So the check is declared outside the engine (a file next to the task, mounted
// read-only), its body is opaque text here, and execution goes through the same
// external channel the model already uses — `execute_code` and its configured
// sandbox provider. The only protocol added to the engine is "exit 0 = pass".
const CompletionCheckSuffix = ".check.py"

// CompletionCheck is one declared check bound to one declared artefact.
type CompletionCheck struct {
	Artifact string
	Source   string
}

// LoadCompletionChecks returns the checks declared for the given artefacts.
//
// A missing, unreadable or empty file means "no check", never "fail": a broken
// declaration is an operator error and must not be reported to the model as a
// defect in its work.
func LoadCompletionChecks(dir string, artifacts []string) []CompletionCheck {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	var out []CompletionCheck
	seen := make(map[string]bool, len(artifacts))
	for _, a := range artifacts {
		base := filepath.Base(strings.TrimSpace(a))
		if base == "" || base == "." || seen[base] {
			continue
		}
		seen[base] = true
		data, err := os.ReadFile(filepath.Join(dir, base+CompletionCheckSuffix)) //nolint:gosec // operator-supplied directory
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		out = append(out, CompletionCheck{Artifact: base, Source: string(data)})
	}
	return out
}

// CompletionCheckProgram prefixes the artefact name so one check body can serve
// several artefacts. `execute_code` runs a program, not a command with argv, and
// carries no environment of its own — the prefix is how the name arrives.
func CompletionCheckProgram(c CompletionCheck) string {
	return fmt.Sprintf("ARTIFACT = %q\n", c.Artifact) + c.Source
}
