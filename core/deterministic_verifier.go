package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// DeterministicVerifier defines the zero-token hard verification contract.
// Implementations must be fast (milliseconds), side-effect-free (read-only),
// and never call an LLM. See docs/TIERED_SHORT_CIRCUIT_AUDIT.md
type DeterministicVerifier interface {
	Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error)
}

// deterministicVerifiers is the registry of all hard-check verifiers.
// Keyed by check.Type. Extend by adding new implementations before init.
var deterministicVerifiers = map[string]DeterministicVerifier{
	"file_exists":     &FileExistsVerifier{},
	"command_pass":    &CommandPassVerifier{},
	"plan_checkboxes": &PlanCheckboxVerifier{},
	"code_entities":   &CodeEntityVerifier{},
}

// FileExistsVerifier checks that a file exists and is non-empty.
// Params: { "path": "<relative or absolute path>" }
type FileExistsVerifier struct{}

func (v *FileExistsVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	path, _ := criteria["path"].(string)
	if path == "" {
		return false, "missing 'path' parameter", nil
	}
	absPath := path
	if !filepath.IsAbs(absPath) && workdir != "" {
		absPath = filepath.Join(workdir, path)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return false, fmt.Sprintf("file not found: %s", path), nil
	}
	if info.Size() == 0 {
		return false, fmt.Sprintf("file is empty: %s", path), nil
	}
	return true, "", nil
}

// CommandPassVerifier checks that a shell command exits with code 0.
// Params: { "command": "<shell command>" }
// Uses sh -c on Unix, cmd /c on Windows.
type CommandPassVerifier struct{}

func (v *CommandPassVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	cmdStr, _ := criteria["command"].(string)
	if cmdStr == "" {
		return false, "missing 'command' parameter", nil
	}

	// audit S-C1: reject shell metacharacters to prevent command injection.
	// audit L-HIGH-3: added % and " for Windows cmd /c variable expansion.
	for _, ch := range cmdStr {
		if ch == ';' || ch == '|' || ch == '&' || ch == '$' || ch == '`' ||
			ch == '>' || ch == '<' || ch == '\n' || ch == '\r' ||
			ch == '%' || ch == '"' {
			return false, fmt.Sprintf("command rejected: contains shell metacharacter %q (audit S-C1)", ch), nil
		}
	}

	// audit S-C1: executable allowlist. Parse the command and run the
	// executable DIRECTLY (no shell) — eliminates sh -c / cmd /c injection.
	// Allowlist is configurable via VORTEX_COMMAND_ALLOWLIST (comma-separated).
	fields := strings.Fields(cmdStr)
	if len(fields) == 0 {
		return false, "empty command after parsing", nil
	}
	// audit NEW-1: reject path separators in the executable. This forces PATH
	// lookup and prevents /tmp/evil/git from bypassing the "git" allowlist
	// (filepath.Base strips the prefix, but exec uses the full path).
	if strings.ContainsAny(fields[0], `/\`) {
		return false, fmt.Sprintf("command rejected: path separators not allowed in executable (audit NEW-1); use a bare command name"), nil
	}
	exe := fields[0] // guaranteed bare name — no path prefix possible
	allowed := map[string]bool{
		"test": true, "echo": true, "cat": true, "ls": true, "grep": true,
		"git": true, "go": true, "python3": true, "python": true, "node": true,
		"npm": true, "make": true, "diff": true, "wc": true, "head": true,
		"tail": true, "sort": true, "uniq": true, "find": true, "true": true,
	}
	if custom := os.Getenv("VORTEX_COMMAND_ALLOWLIST"); custom != "" {
		allowed = make(map[string]bool)
		for _, c := range strings.Split(custom, ",") {
			allowed[strings.TrimSpace(c)] = true
		}
	}
	if !allowed[exe] {
		return false, fmt.Sprintf("command rejected: executable %q not in allowlist (audit S-C1)", exe), nil
	}

	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	// On Windows, cmd.exe builtins (echo, type, etc.) have no standalone
	// executable. If LookPath fails, fall back to cmd /c. Safe because
	// metacharacters are already rejected (audit S-C1).
	if runtime.GOOS == "windows" {
		if _, lookErr := exec.LookPath(fields[0]); lookErr != nil {
			cmd = exec.CommandContext(ctx, "cmd", append([]string{"/c"}, fields...)...)
			if workdir != "" {
				cmd.Dir = workdir
			}
		}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("command failed: %s\noutput: %s", err, string(output)), nil
	}
	return true, "", nil
}

// runDeterministicChecks executes all structured deterministic checks for a step.
// Returns (passed, failureMessage, failureType). If all pass, returns (true, "", "").
// workdir is the base directory for relative path resolution (typically s.outputBase).
func runDeterministicChecks(ctx context.Context, workdir string, checks []schemas.DeterministicCheck) (bool, string, string) {
	for _, check := range checks {
		verifier, ok := deterministicVerifiers[check.Type]
		if !ok {
			continue
		}
		passed, msg, err := verifier.Verify(ctx, workdir, check.Params)
		if err != nil || !passed {
			failType := "logic_error"
			if check.Type == "file_exists" {
				failType = "schema_violation"
			}
			return false, fmt.Sprintf("deterministic check [%s] failed: %s", check.Type, msg), failType
		}
	}
	return true, "", ""
}

// PlanCheckboxVerifier checks that all checkboxes in a step plan file are
// marked as done (- [x]). This is a soft check — it passes if there are no
// checkboxes at all (the plan used a numbered list instead).
//
// Params: { "path": "<relative or absolute path to plan .md file>" }
// Passes if: file exists AND (no checkboxes found OR all checkboxes are [x]).
// Fails if: file not found OR any checkbox is still unchecked (- [ ]).
//
// Design: STEP_PLAN_MODE_DESIGN.md §2.4 item #5 (checkbox-based plan completion).
type PlanCheckboxVerifier struct{}

func (v *PlanCheckboxVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	path, _ := criteria["path"].(string)
	if path == "" {
		return false, "missing 'path' parameter", nil
	}
	absPath := path
	if !filepath.IsAbs(absPath) && workdir != "" {
		absPath = filepath.Join(workdir, path)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false, fmt.Sprintf("plan file not found: %s", path), nil
	}
	content := string(data)
	// Count unchecked vs checked checkboxes.
	// Supports both "- [ ]" (unchecked) and "- [x]" / "- [X]" (checked).
	unchecked := strings.Count(content, "- [ ]")
	totalBoxes := unchecked + strings.Count(content, "- [x]") + strings.Count(content, "- [X]")
	if totalBoxes == 0 {
		return true, "no checkboxes in plan (numbered list format)", nil
	}
	if unchecked > 0 {
		return false, fmt.Sprintf("%d/%d plan checkboxes still unchecked", unchecked, totalBoxes), nil
	}
	return true, fmt.Sprintf("all %d plan checkboxes completed", totalBoxes), nil
}

// CodeEntityVerifier (#17, BS Detector) checks that code entities (function,
// type, or variable names) referenced in agent output actually exist in the
// codebase. This is a zero-LLM-cost, zero-false-positive grep-style check.
//
// Params:
//   "entities":   []string — entity names to verify (e.g. ["foo", "BarBaz"])
//   "search_dir": string   — directory to search (optional, defaults to workdir)
//   "pattern":    string   — optional search pattern prefix (default: "func %s|type %s|var %s")
//
// Passes if: all entities are found in at least one .go file under search_dir.
// Fails if: any entity is not found anywhere.
//
// Design: docs/plan/STEP_PLAN_MODE_DESIGN.md §13.6 (item #17, Intrect-io BS Detector).
type CodeEntityVerifier struct{}

func (v *CodeEntityVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	entitiesRaw, _ := criteria["entities"].([]any)
	if len(entitiesRaw) == 0 {
		return false, "missing or empty 'entities' parameter", nil
	}
	searchDir, _ := criteria["search_dir"].(string)
	if searchDir == "" {
		searchDir = workdir
	}
	if searchDir == "" {
		return false, "no search directory (neither search_dir nor workdir set)", nil
	}

	var missing []string
	verifiedCount := 0
	// audit P-HIGH-1: walk tree once and collect all .go file contents,
	// then check all entities against the collected content — O(tree_walk)
	// instead of O(N × tree_walk).
	fileContents := collectGoFiles(searchDir)
	for _, e := range entitiesRaw {
		name, _ := e.(string)
		if name == "" {
			continue
		}
		verifiedCount++
		if !entityFound(fileContents, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("code entities not found: %s", strings.Join(missing, ", ")), nil
	}
	// audit L-LOW-2: report actual verified count, not len(entitiesRaw)
	// which includes skipped empty/non-string entries.
	return true, fmt.Sprintf("all %d code entities verified", verifiedCount), nil
}

// collectGoFiles walks dir recursively and returns the contents of all .go
// files, skipping vendor/.git/node_modules/trash directories.
func collectGoFiles(dir string) []string {
	var contents []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// audit L-MED-3: log walk errors for observability — previously
			// silently ignored, causing false negatives with no indication.
			fmt.Fprintf(os.Stderr, "[code-entity-verifier] walk error at %s: %v\n", path, err)
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == ".git" || base == "node_modules" || base == "trash" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			contents = append(contents, string(data))
		} else {
			// audit L-MED-3: log read errors for observability.
			fmt.Fprintf(os.Stderr, "[code-entity-verifier] read error %s: %v\n", path, err)
		}
		return nil
	})
	return contents
}

// entityFound checks if any file content contains a declaration of the named
// entity (func/type/var/const, including method declarations with receivers).
func entityFound(contents []string, name string) bool {
	// audit L-HIGH-4: added patterns for method declarations with receivers:
	//   func (recv) Name(   /   func (recv) Name<space>
	// Previously only `func Name(` / `func Name<space>` were checked, so
	// methods like `func (v *T) Verify(...)` were never matched.
	for _, content := range contents {
		if strings.Contains(content, "func "+name+" ") ||
			strings.Contains(content, "func "+name+"(") ||
			strings.Contains(content, "type "+name+" ") ||
			strings.Contains(content, "type "+name+"[") ||
			strings.Contains(content, "var "+name+" ") ||
			strings.Contains(content, "var "+name+"\n") ||
			strings.Contains(content, "const "+name+" ") ||
			strings.Contains(content, "const "+name+"\n") ||
			strings.Contains(content, ") "+name+"(") ||
			strings.Contains(content, ") "+name+" ") {
			return true
		}
	}
	return false
}
