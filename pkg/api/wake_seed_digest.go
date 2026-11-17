package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Wake-turn artifact digest — the fix for D21.
//
// The wake trigger tells the model "Read the artifacts it produced and answer the
// user's original request now" and then hands it a notification that contains a
// verdict and a list of *names*. Measured on harness-bench 041 (2026-11-17, four
// cells, 22 runs): the wake turn made **zero** tool calls in 10/10 wake runs, and
// the answer cited the plan's `total_hourly_cost_usd` in **0/10** of them, versus
// 6/12 in the polling arm where the same instruction is obeyed whenever the model
// had already spent a call on `orchestrator_task_status`.
//
// The failing run scored 1.00: the child writes the deliverable, so the oracle
// never sees the answer. One of them said "$0.782, c6g.medium, us-east-1, 272.5
// RPS required"; the plan on disk said 0.552, c7g.xlarge, us-east, 3108.56.
//
// So the defect is not "the model is lazy" — it is "the turn has nothing to
// quote". This file gives it something, bounded and deterministic, the same way
// a human reviewer would skim a deliverable before reporting it.
//
// Constraints this must respect:
//   - Read-only. The workspace change contract compares the final tree to the
//     starting one; a digest that wrote a cache file would fail every run.
//   - Symlinks are skipped, not followed. A link out of the workspace would put
//     content from outside the round's contract into the prompt.
//   - Bounded. A 200 KB CSV rendered verbatim would spend the whole turn's
//     context on one artifact; the budget is a character count (≈4 chars/token)
//     and truncation is stated in the text, because a silently shortened artifact
//     reads exactly like a short one (iron law 220).
//   - Bounded in bytes as well as characters. The corpus's largest declared
//     artifact is a 45 KB SQLite file, but the read cap exists for the task that
//     is not in today's corpus: a 2 GB log must not be loaded to show 1600 chars.
//   - Binary is reported, not rendered. Task 111 declares
//     `state/hris_payroll.db`; the fallback branch would otherwise paste a prefix
//     of SQLite pages into the prompt, which is noise the model can only
//     misread as text.
const (
	// wakeDigestBudgetChars is the whole digest's budget. 4000 chars ≈ 1000
	// tokens: enough for the headline scalars of a plan plus a header row, small
	// enough that it cannot crowd out the original request.
	wakeDigestBudgetChars = 4000
	// wakeDigestFileChars caps one artifact before the shared budget applies.
	wakeDigestFileChars = 1600
	// wakeDigestCSVRows is how many data rows a CSV contributes. One or two rows
	// is what a reviewer reads to tell a real table from a header.
	wakeDigestCSVRows = 2
	// wakeDigestMaxKeys caps a JSON object's top-level scalar keys.
	wakeDigestMaxKeys = 24
	// wakeDigestReadCap is the most this code will read from any one file.
	wakeDigestReadCap = 64 << 10
	// wakeDigestSniffBytes is how much of a file is inspected for NUL before it
	// is treated as binary. 8 KiB matches the heuristic `git diff` and `file(1)`
	// use, and every binary format that matters declares itself that early.
	wakeDigestSniffBytes = 8 << 10
)

// errArtifactBinary is returned instead of content. The caller reports the name
// and moves on; it is not an error in the round, so it must not abort the digest.
var errArtifactBinary = errors.New("artifact is binary")

// wakeSeedDigestMode reads VORTEX_WAKE_SEED_DIGEST. Default off: the digest
// changes what the model sees at the start of a wake turn, so it must be turned
// on deliberately and measured, exactly like the ETA and watchdog gates.
func wakeSeedDigestMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORTEX_WAKE_SEED_DIGEST"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// wakeArtifactDigest renders the digest for a finished task, or "" when the gate
// is off or there is nothing to show. It never returns an error: a wake that
// cannot read the artifacts must still start (that is the pre-fix behaviour, and
// it is better than no wake at all), with the reason stated in the text so the
// next reader does not have to guess why the block is short.
func wakeArtifactDigest(o taskOutcome) string {
	if !wakeSeedDigestMode() {
		return ""
	}
	if o.WorkspaceRoot == "" || len(o.Declared) == 0 {
		return ""
	}
	digest, skipped := buildArtifactDigest(o.WorkspaceRoot, o.Declared, wakeDigestBudgetChars)
	// A skipped name is not injected into the prompt when nothing could be
	// rendered — the task notification already says the artifacts are absent, and
	// a second paragraph repeating it adds no information. It is still logged: an
	// escape attempt ("…/../etc/passwd") and a missing deliverable are different
	// events, and only one of them is a bug in the delegated prompt.
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "[wake-seed] task=%s digest skipped: %s\n",
			o.ID, strings.Join(skipped, ", "))
	}
	return digest
}

// buildArtifactDigest is the pure half, so the rendering rules can be tested
// without a handler. The second return value names what was dropped, so a caller
// can report it instead of letting a short block read like a complete one.
func buildArtifactDigest(root string, declared []string, budget int) (string, []string) {
	rootClean, err := filepath.Abs(root)
	if err != nil {
		return "", nil
	}
	var b strings.Builder
	header := "\n\n## Artifact digest (read by the controller — the files are authoritative)\n"
	b.WriteString(header)

	rendered := 0
	var skipped []string
	for _, name := range declared {
		path, ok := resolveArtifactPath(rootClean, name)
		if !ok {
			skipped = append(skipped, name+" (outside the workspace)")
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			skipped = append(skipped, name+" (missing)")
			continue
		}
		// Lstat + ModeSymlink: a link is reported, never followed. Reading through
		// it would pull content the round's contract does not cover.
		if info.Mode()&os.ModeSymlink != 0 {
			skipped = append(skipped, name+" (symlink, not followed)")
			continue
		}
		if info.IsDir() {
			skipped = append(skipped, name+" (directory)")
			continue
		}
		body, note, err := renderArtifact(path, name)
		if errors.Is(err, errArtifactBinary) {
			// Reported, not rendered: pasting SQLite pages into a prompt invites
			// the model to read them as text.
			skipped = append(skipped, name+" (binary, not shown)")
			continue
		}
		if err != nil {
			skipped = append(skipped, name+" (unreadable)")
			continue
		}
		body = strings.TrimRight(body, "\n")
		remaining := budget - b.Len()
		if remaining <= 0 {
			skipped = append(skipped, name+" (budget exhausted)")
			continue
		}
		if len(body) > remaining {
			cut := remaining - 40
			if cut < 0 {
				cut = 0
			}
			body = body[:cut] + "\n…(truncated by the digest budget)"
		}
		if len(body) > wakeDigestFileChars {
			body = body[:wakeDigestFileChars] + "\n…(truncated per file)"
		}
		// The note goes on *after* truncation. Appended by renderArtifact it would
		// be cut off by the very truncation it describes — the reader would see a
		// short file and no reason for it.
		if note != "" {
			body += "\n" + note
		}
		rendered++
		b.WriteString("\n### " + name + "\n")
		b.WriteString(body + "\n")
	}

	if rendered == 0 {
		// No stub: the caller's notification already says the artifacts are
		// missing, and an empty "## Artifact digest" heading with nothing under it
		// invites the model to fill the gap from priors — the original defect.
		return "", skipped
	}
	if len(skipped) > 0 {
		b.WriteString("\nNot shown: " + strings.Join(skipped, ", ") + ".\n")
	}
	b.WriteString("\nThese excerpts are for quoting figures accurately; open a file when you need the parts that were cut.\n")
	return b.String(), skipped
}

// resolveArtifactPath joins a declared name under root and refuses anything that
// escapes it. Declared names come from the delegated prompt, i.e. from model
// output, so "…/../etc/passwd" is a real input, not a hypothetical one.
func resolveArtifactPath(rootClean, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	rel := filepath.Clean(name)
	if filepath.IsAbs(rel) {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	full := filepath.Join(rootClean, rel)
	clean, err := filepath.Abs(full)
	if err != nil {
		return "", false
	}
	if clean != rootClean && !strings.HasPrefix(clean, rootClean+string(os.PathSeparator)) {
		return "", false
	}
	return clean, true
}

// renderArtifact picks a rendering by extension. JSON and CSV get structure-aware
// forms because those are the two that carry the numbers an answer is judged on;
// everything else is a prefix.
//
// The second return value is a trailing note (e.g. "only the first N bytes were
// read") that the caller must append *after* it applies the character caps — a
// note attached here would be truncated together with the body it explains.
func renderArtifact(path, name string) (body, note string, err error) {
	data, size, err := readArtifactPrefix(path)
	if err != nil {
		return "", "", err
	}
	if data == nil {
		return "", "", errArtifactBinary
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		body = renderJSON(data)
	case ".csv":
		body = renderCSV(data, ',')
	case ".tsv":
		body = renderCSV(data, '\t')
	default:
		body = string(data)
	}
	if size > int64(len(data)) {
		// A prefix that stops mid-sentence reads like a short artifact, so the
		// reason travels with it (iron law 220).
		note = fmt.Sprintf("(file is %d bytes; only the first %d were read)", size, len(data))
	}
	return body, note, nil
}

// readArtifactPrefix reads at most wakeDigestReadCap bytes and decides whether the
// file is binary. A nil slice with a nil error means binary; the size is returned
// either way so the caller can state that it saw only a prefix.
func readArtifactPrefix(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	buf := make([]byte, wakeDigestReadCap)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, 0, err
	}
	buf = buf[:n]
	sniff := buf
	if len(sniff) > wakeDigestSniffBytes {
		sniff = sniff[:wakeDigestSniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return nil, info.Size(), nil
	}
	// A latin-1 file is text but not valid UTF-8; the prompt is JSON, and
	// U+FFFD is the only honest way to carry the byte through.
	if !utf8.Valid(buf) {
		buf = []byte(strings.ToValidUTF8(string(buf), "\uFFFD"))
	}
	return buf, info.Size(), nil
}

// renderJSON prints the top-level scalars, which is where a plan puts its
// headline (total cost, selected type, required capacity). Nested values are
// described by shape rather than dumped: the point is to give the answer
// something to quote, not to replace the file.
func renderJSON(data []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		// An array or a malformed file: show the prefix so the block is not empty.
		s := string(data)
		if len(s) > wakeDigestFileChars {
			s = s[:wakeDigestFileChars] + "…"
		}
		return s
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	n := 0
	for _, k := range keys {
		if n >= wakeDigestMaxKeys {
			b.WriteString(fmt.Sprintf("…(%d more keys)\n", len(keys)-n))
			break
		}
		switch v := obj[k].(type) {
		case []any:
			b.WriteString(fmt.Sprintf("%s: [%d item(s)]\n", k, len(v)))
			if len(v) > 0 {
				if first, ok := v[0].(map[string]any); ok {
					b.WriteString("  first: " + compactScalars(first) + "\n")
				}
			}
		case map[string]any:
			b.WriteString(fmt.Sprintf("%s: {%d key(s)}\n", k, len(v)))
		default:
			b.WriteString(fmt.Sprintf("%s: %s\n", k, scalarText(v)))
		}
		n++
	}
	return b.String()
}

// compactScalars renders one nested object as "k=v, k=v" so the first element of
// a service list is quotable without dumping the whole array.
func compactScalars(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any, map[string]any:
			continue
		default:
			parts = append(parts, k+"="+scalarText(v))
		}
	}
	return strings.Join(parts, ", ")
}

func scalarText(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if t == "" {
			return `""`
		}
		if len(t) > 120 {
			t = t[:120] + "…"
		}
		return t
	case float64:
		// JSON numbers arrive as float64; %v gives 0.552 rather than 0.552000.
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

// renderCSV shows the header plus a couple of rows, with a total row count so the
// reader knows the excerpt is an excerpt.
func renderCSV(data []byte, comma rune) string {
	r := csv.NewReader(strings.NewReader(string(data)))
	r.Comma = comma
	r.FieldsPerRecord = -1 // rows in a repair task are exactly what is malformed
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil || len(rows) == 0 {
		s := string(data)
		if len(s) > wakeDigestFileChars {
			s = s[:wakeDigestFileChars] + "…"
		}
		return s
	}
	var b strings.Builder
	for i, row := range rows {
		if i > wakeDigestCSVRows {
			b.WriteString(fmt.Sprintf("…(%d more row(s))\n", len(rows)-i))
			break
		}
		b.WriteString(strings.Join(row, string(comma)) + "\n")
	}
	return b.String()
}
