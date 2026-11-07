package core

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// Declared artifacts: the completion contract the task prompt hands us for free.
//
// The benchmark controller appends a "## Enforced workspace change contract"
// section to every task prompt, listing the files the workspace must contain at
// the end. The engine does not need a new parameter to learn them — it only
// needs to read the task prompt it was already given.
//
// Why this matters: without a notion of "the work is done", the only exit is
// the turn budget, and 4 runs out of 133 hit the 150-turn cap. Those runs are
// also the ones that eat the token budget (3% of runs, 26% of tokens). The
// declared list is the cheapest available definition of "done".
//
// Two rules from the sample (prompts/round-1.txt:58) drive the parsing:
//
//   - Only the section counts. The same prompt references
//     `/workspace/customer_metrics.csv` in prose at lines 12 and 17; a
//     whole-text backtick scan would pick those up and also invent artifacts
//     from unrelated examples.
//   - Only "- Create:" lines count. "- Modify: none" and "- Delete: none" are
//     real lines in the section whose content is the word "none", not a file.
const contractSectionHeading = "## Enforced workspace change contract"

// DeclaredContract is the same section split by column.
//
// Only Create names artifacts (see DeclaredContract.Create). The other two are
// here for a different question — "which paths may this workspace hold at the
// end" — which an extras check asks and a presence check cannot answer:
//
//   - Modify names a file the seed already ships. It must still be there and it
//     must have changed. Its presence proves nothing (→221), which is exactly
//     why it cannot enter the artifact list, and exactly why it can be part of
//     the allowed set.
//   - Delete names a path that must be gone; that absence is never "an extra".
//
// Present distinguishes "the prompt has no contract section" from "the section
// names nothing to create" — the distinction →219 recorded as missing from the
// older API. It has a consumer now (the extras gate), so it is not speculative.
type DeclaredContract struct {
	Present bool
	// Create is the only column that names artifacts, and the only one a
	// completion check may read. Modify is excluded on measurement, not taste:
	// the three tasks whose contract is "Create: none / Modify: X" (262/263/264)
	// ship X in the seed workspace, so an existence check on X answers "done" at
	// t=0 -- and the harness fires the completion marker on missing == 0 alone,
	// which would tell the model it is finished on turn one and spend the
	// one-shot latch. Those tasks therefore get no completion signal from this
	// section at all: "modified, not created" is unobservable by a presence test
	// (TestWorkspaceArtifactsComplete_ModifyTargetIsPresentAtStart, RESULT §28.7).
	//
	// nil means "nothing to create" -- true both when the prompt has no section
	// and when the section says so. Callers gate on len(...) > 0, so the two
	// behave identically; Present tells them apart when that matters.
	Create []string
	// Modify lists files the seed already ships: they must still be there and
	// must have changed. Presence proves nothing (→221), which is why they are
	// excluded from Create and why they belong to the allowed set an extras check
	// compares against.
	Modify []string
	// Delete lists paths that must be gone; their absence is never "an extra".
	Delete []string
}

// ParseDeclaredContract reads all three columns of the contract section.
//
// Same narrow rules as the artifact parser: only the section counts, and a
// column whose content is the word "none" contributes nothing because "none"
// carries no backticks. Order is first-seen, duplicates dropped per column.
func ParseDeclaredContract(text string) DeclaredContract {
	section := contractSection(text)
	if section == "" {
		return DeclaredContract{}
	}

	out := DeclaredContract{Present: true}
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- Create:"):
			out.Create = appendMissing(out.Create, backtickedTokens(trimmed))
		case strings.HasPrefix(trimmed, "- Modify:"):
			out.Modify = appendMissing(out.Modify, backtickedTokens(trimmed))
		case strings.HasPrefix(trimmed, "- Delete:"):
			out.Delete = appendMissing(out.Delete, backtickedTokens(trimmed))
		}
	}
	return out
}

// appendMissing appends tokens not already in dst, preserving first-seen order.
func appendMissing(dst, tokens []string) []string {
	for _, token := range tokens {
		seen := false
		for _, existing := range dst {
			if existing == token {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, token)
		}
	}
	return dst
}

// contractSection returns the text between the contract heading and the next
// "## " line, or "" when the heading is absent.
//
// The heading is matched exactly (after trimming) rather than by substring:
// a prompt that says "## Enforced workspace change contract (legacy)" describes
// a format this parser was not written against, and silently half-parsing it
// would be worse than returning no contract at all.
func contractSection(text string) string {
	lines := strings.Split(text, "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == contractSectionHeading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}

	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// backtickedTokens returns the contents of every `...` span in s, in order.
func backtickedTokens(s string) []string {
	var out []string
	for {
		open := strings.IndexByte(s, '`')
		if open < 0 {
			return out
		}
		rest := s[open+1:]
		close := strings.IndexByte(rest, '`')
		if close < 0 {
			return out
		}
		if token := strings.TrimSpace(rest[:close]); token != "" {
			out = append(out, token)
		}
		s = rest[close+1:]
	}
}

// MissingArtifacts returns the declared names that are not on disk, in
// declaration order.
//
// It stats, it does not read: presence is the only question here, and deciding
// whether a CSV's *contents* are good would put a second, worse grader inside
// the engine. A zero-byte file counts as present — it is a real failure mode,
// but a different one, and the scorer already judges it.
//
// The name is resolved strictly against root (see resolveArtifactPath): a task
// that declared `x.md` and got `<root>/workspace/x.md` is reported missing.
// That is the signal the delegation receipt needs, and it must NOT be softened
// into a recursive search — finding the file somewhere else turns a silent
// misplacement into a "looks complete".
func MissingArtifacts(root string, declared []string) []string {
	var missing []string
	// Names that would escape root are counted as missing rather than followed.
	// The contract comes from task text, which on a hostile input is untrusted.
	for _, rel := range declared {
		path, ok := resolveArtifactPath(root, rel)
		if !ok {
			missing = append(missing, rel)
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			continue
		}
		missing = append(missing, rel)
	}
	return missing
}

// WorkspaceArtifactsComplete counts how many declared artifacts exist under
// root. Thin wrapper over MissingArtifacts so the presence rule has exactly one
// implementation (→131: a second copy would drift).
func WorkspaceArtifactsComplete(root string, declared []string) (present, missing int) {
	absent := MissingArtifacts(root, declared)
	return len(declared) - len(absent), len(absent)
}

// ── Extras: what the workspace grew that the contract never allowed ──

// workspaceWalkSkip lists directories whose contents are never deliverables.
// They are skipped rather than ignored after the fact: a workspace with a
// materialised node_modules would otherwise cost thousands of stat calls per
// check, and .git is not part of what any task hands in.
var workspaceWalkSkip = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
}

// maxWorkspaceEntries bounds the walk. A workspace that exceeds it is not a
// deliverable anyone is grading by enumeration, and returning an error (rather
// than a truncated set) keeps the caller from reporting a wrong answer
// (→201: a partial view must not stand in for the whole).
const maxWorkspaceEntries = 20000

// SnapshotWorkspace records every path under root, relative and slash-separated.
//
// This is the t=0 baseline an extras check compares against. It has to be taken
// before the model's first turn, because "the workspace grew a file" is only
// answerable relative to what was there when the run started — and on the tasks
// this exists for, the deliverable itself is part of that starting state (→221).
func SnapshotWorkspace(root string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, 64)
	err := walkWorkspace(root, func(rel string) error {
		if len(out) >= maxWorkspaceEntries {
			return fmt.Errorf("workspace has more than %d entries; extras check disabled", maxWorkspaceEntries)
		}
		out[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WorkspaceNewPaths returns the paths under root that baseline does not have,
// sorted, so the same workspace always yields the same message.
//
// Directories count. A task that says "Create: none" and means it does not want
// a `logs/` directory either, and the scorer's own workspace_scope compares
// directory entries as entries. Paths are not filtered by kind for the same
// reason MissingArtifacts stats rather than reads: the question is presence.
func WorkspaceNewPaths(root string, baseline map[string]struct{}) ([]string, error) {
	var out []string
	err := walkWorkspace(root, func(rel string) error {
		if _, known := baseline[rel]; !known {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// walkWorkspace visits every path under root once, calling visit with the
// slash-separated relative path. One implementation so the snapshot and the diff
// cannot disagree about what counts as a path (→131).
func walkWorkspace(root string, visit func(rel string) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if d.IsDir() && workspaceWalkSkip[d.Name()] {
			return fs.SkipDir
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		return visit(filepath.ToSlash(rel))
	})
}

// resolveArtifactPath maps a declared name onto root, refusing anything that
// points outside it.
func resolveArtifactPath(root, declared string) (string, bool) {
	rel := strings.TrimSpace(declared)
	// The prompts write bare names, but tolerate the absolute-looking form that
	// appears in prose: "/workspace/x.csv" means x.csv under root.
	rel = strings.TrimPrefix(rel, "/workspace/")
	rel = strings.TrimPrefix(rel, "workspace/")
	rel = strings.TrimPrefix(rel, "/")

	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == "" || filepath.IsAbs(clean) {
		return "", false
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(root, clean), true
}

// ── Declared fields: the prompt also says what each JSON artifact must contain ──

// createContainingSuffix marks the block that lists an artifact's top-level
// keys. 011's prompt is the shape this parses:
//
//	Create `$WORKSPACE/scheduling_plan.json` containing:
//
//	- `request_id`, `status`, and `timezone`;
//	- one `recommended` candidate and two ordered `fallbacks`;
//	- for each candidate: `start`, `end`, and a concise `selection_reason`;
//
// The last bullet is why this cannot be a blanket backtick scan: `start` and
// `end` are fields of a *candidate*, not of the document. A bullet whose first
// colon comes before its first backtick is a scoped bullet ("for each X:",
// "per row:") and is skipped. The rule is narrow on purpose — a false positive
// here becomes a "you are missing a key the task never asked for" steer, which
// is worse than not checking at all (→107: a check that fires on the wrong
// thing is indistinguishable from a broken check).
const createContainingSuffix = "containing:"

// ParseDeclaredArtifactFields returns, per declared artifact, the top-level keys
// the prompt says it must carry. Keys are normalised to workspace-relative form
// so they line up with DeclaredContract.Create's names.
//
// Returns nil when the prompt declares no such block — nil must mean "no
// contract", never "empty contract", so callers keep their previous behaviour.
func ParseDeclaredArtifactFields(text string) map[string][]string {
	lines := strings.Split(text, "\n")
	out := make(map[string][]string)

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "Create ") || !strings.HasSuffix(trimmed, createContainingSuffix) {
			continue
		}
		tokens := backtickedTokens(trimmed)
		if len(tokens) == 0 {
			continue
		}
		artifact := normalizeDeclaredName(tokens[len(tokens)-1])
		if artifact == "" {
			continue
		}

		var fields []string
		seen := make(map[string]struct{})
		started := false
		for j := i + 1; j < len(lines); j++ {
			line := strings.TrimSpace(lines[j])
			if line == "" {
				if !started {
					continue // the prompt puts a blank line right after "containing:"
				}
				break // a blank line after the bullets closes the block
			}
			started = true
			if !strings.HasPrefix(line, "- ") {
				continue // wrapped continuation of the previous bullet
			}
			body := strings.TrimPrefix(line, "- ")
			colon, tick := strings.IndexByte(body, ':'), strings.IndexByte(body, '`')
			if colon >= 0 && (tick < 0 || colon < tick) {
				continue // scoped bullet: "for each candidate: `start`, ..."
			}
			for _, token := range backtickedTokens(body) {
				if !isJSONKey(token) {
					continue
				}
				if _, dup := seen[token]; dup {
					continue
				}
				seen[token] = struct{}{}
				fields = append(fields, token)
			}
		}
		if len(fields) > 0 {
			out[artifact] = fields
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeDeclaredName strips the workspace prefix a prompt may write in front
// of an artifact name, so the result matches the bare names in the contract
// section.
func normalizeDeclaredName(name string) string {
	rel := strings.TrimSpace(name)
	for _, prefix := range []string{"$WORKSPACE/", "/workspace/", "workspace/", "/"} {
		if strings.HasPrefix(rel, prefix) {
			rel = strings.TrimPrefix(rel, prefix)
		}
	}
	return rel
}

// isJSONKey reports whether s could be a JSON object key written as an
// identifier in prose. Anything with a dot, slash or space is a filename or a
// phrase, not a key.
func isJSONKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// WorkspaceArtifactDefects reports every way the declared artifacts violate the
// field contract the prompt stated for them: not a JSON object, a missing
// top-level key, a key with the wrong JSON type.
//
// This is the "exists but wrong" half. WorkspaceArtifactsComplete answers "is it
// there"; a file can be there and still be the wrong shape, which is the failure
// that cost 4 of 11 v71 runs on harness-bench 011 (`candidates: [{type:
// "recommended", …}]` instead of a top-level `recommended`) and 5 more a
// participant map keyed by name instead of email. Existence alone called all of
// them done.
//
// Read-only, no model call, O(fields). Missing files are left to
// WorkspaceArtifactsComplete so a single absence is not reported twice.
func WorkspaceArtifactDefects(root string, fields map[string][]string) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var defects []string
	for _, name := range names {
		path, ok := resolveArtifactPath(root, name)
		if !ok {
			continue
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue // absence has its own message
		}
		contract := schemas.ArtifactContract{Path: path, RequiredFields: fields[name]}
		for _, problem := range InspectArtifact(contract) {
			defects = append(defects, describeArtifactProblem(name, problem))
		}
	}
	return defects
}

// describeArtifactProblem renders one problem as a line a model can act on.
func describeArtifactProblem(name string, problem ArtifactProblem) string {
	switch problem.Reason {
	case ProblemNotJSON:
		return fmt.Sprintf("%s is not a JSON object", name)
	case ProblemMissing:
		return fmt.Sprintf("%s is missing %q", name, problem.Field)
	case ProblemTypeSchemaAbsent:
		return fmt.Sprintf("%s is missing %q", name, problem.Field)
	case ProblemWrongType:
		return fmt.Sprintf("%s field %q has the wrong type (%s)", name, problem.Field, problem.Detail)
	default:
		return fmt.Sprintf("%s cannot be read", name)
	}
}
