package core

import (
	"fmt"
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

// ParseDeclaredArtifacts extracts the artifact names a task promises to create.
//
// Returns nil when the prompt carries no contract section — that is the
// ordinary case for every caller outside the benchmark, and nil must mean
// "no contract", never "empty contract". Callers treat nil as "behave exactly
// as before"; only a non-empty result may change behaviour.
func ParseDeclaredArtifacts(text string) []string {
	section := contractSection(text)
	if section == "" {
		return nil
	}

	seen := make(map[string]struct{})
	var out []string
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- Create:") {
			continue
		}
		for _, token := range backtickedTokens(trimmed) {
			if _, dup := seen[token]; dup {
				continue
			}
			seen[token] = struct{}{}
			out = append(out, token)
		}
	}
	return out
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
// so they line up with ParseDeclaredArtifacts' names.
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
