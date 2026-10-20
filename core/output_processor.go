package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"gopkg.in/yaml.v3"
)

// OutputProcessor handles the transformation and summarization of tool results.
type OutputProcessor struct {
	MaxBodyChars int
}

// defaultMaxBodyChars is the hardcoded fallback when neither explicit config
// nor model context-window info is available. Preserves pre-2026-10-17
// behavior (3000 chars ≈ 750 tokens) as a zero-change floor.
const defaultMaxBodyChars = 3000

// NewOutputProcessor creates an OutputProcessor with the given max body chars.
// If maxBodyChars <= 0, falls back to defaultMaxBodyChars (3000).
// Callers should pass getMaxToolResultChars(registry) for model-aware defaults.
func NewOutputProcessor(maxBodyChars int) *OutputProcessor {
	if maxBodyChars <= 0 {
		maxBodyChars = defaultMaxBodyChars
	}
	return &OutputProcessor{
		MaxBodyChars: maxBodyChars,
	}
}

// SetMaxBodyChars updates the truncation limit. Used by Spawner.SetModelRegistry
// to apply a model-registry-derived dynamic default after the model registry is
// wired (which happens post-construction). Thread-safe via the Spawner's
// guarantee that SetModelRegistry is called once during startup before any
// tool execution begins.
func (p *OutputProcessor) SetMaxBodyChars(n int) {
	if n > 0 {
		p.MaxBodyChars = n
	}
}

// getMaxToolResultChars resolves the tool result truncation limit.
// Priority: (1) explicit config override registry.System.MaxToolResultChars,
// (2) dynamic default from the default provider's MaxContextWindow
//
//	(8% of context window × 4 chars/token, clamped [4000, 32000]),
//
// (3) hardcoded fallback 3000 (preserves legacy behavior).
// This mirrors the getMaxSafeTaskChars pattern in input_guard.go.
func getMaxToolResultChars(reg *config.Registry) int {
	if reg == nil {
		return defaultMaxBodyChars
	}
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	// (1) Explicit config override.
	if reg.System.MaxToolResultChars > 0 {
		return reg.System.MaxToolResultChars
	}

	// (2) Dynamic default from provider's MaxContextWindow.
	if reg.DefaultProvider != "" && reg.Providers != nil {
		if p, ok := reg.Providers[reg.DefaultProvider]; ok && p != nil && p.MaxContextWindow > 0 {
			return computeDynamicMaxBodyChars(p.MaxContextWindow)
		}
	}

	// (3) Fallback.
	return defaultMaxBodyChars
}

// computeDynamicMaxBodyChars calculates a model-aware tool result char limit
// from the model's context window size (in tokens).
// Formula: clamp(contextWindow × 0.08 × 4, 4000, 32000)
//   - 8% of context window (between Aider's 6.25% and 12.5%)
//   - × 4 chars/token (standard approximation)
//   - min 4000 chars (1000 tokens), max 32000 chars (8000 tokens)
//
// Examples: 8K→4000, 32K→10240, 128K→32000.
func computeDynamicMaxBodyChars(contextWindowTokens int) int {
	if contextWindowTokens <= 0 {
		return defaultMaxBodyChars
	}
	const (
		fraction    = 0.08
		charsPerTok = 4
		minChars    = 4000
		maxChars    = 32000
	)
	v := int(float64(contextWindowTokens) * fraction * float64(charsPerTok))
	if v < minChars {
		return minChars
	}
	if v > maxChars {
		return maxChars
	}
	return v
}

// Wrap converts a raw tool result into a structured SemanticEnvelope.
func (p *OutputProcessor) Wrap(toolName string, raw any, status string, source string, reason string) *schemas.SemanticEnvelope {
	env := &schemas.SemanticEnvelope{
		Status:    status,
		Source:    source,
		Timestamp: time.Now().Unix(),
		Reason:    reason,
	}

	// Determine type and content
	switch v := raw.(type) {
	case string:
		env.Type = "text"
		env.Content = p.truncate(v)
	case []byte:
		env.Type = "text"
		env.Content = p.truncate(string(v))
	default:
		env.Type = "json"
		data, _ := json.MarshalIndent(v, "", "  ")
		env.Content = p.truncate(string(data))
	}

	// Extract citations (e.g., file paths, IDs)
	env.Citations = p.extractCitations(env.Content)

	return env
}

func (p *OutputProcessor) truncate(s string) string {
	if len(s) <= p.MaxBodyChars {
		return s
	}

	// JSON-aware path: keeps the payload parseable. This is not cosmetic -- a
	// JSON document cut at an arbitrary byte offset cannot be parsed at all, so
	// the model receives noise rather than a shorter answer.
	if truncated, ok := truncateJSONFields(s, p.MaxBodyChars); ok {
		return truncated
	}

	// Newline-delimited JSON (JSONL/NDJSON) is a different shape and needs its
	// own cut: it is not one document, so the previous attempt above rejects it,
	// and slicing it at an arbitrary byte offset breaks every line that
	// straddles the cut.
	if truncated, ok := truncateJSONLines(s, p.MaxBodyChars); ok {
		return truncated
	}

	// Fallback for content that genuinely is not JSON: CLI stdout, plain text,
	// stack traces. head+tail is the right shape there because the informative
	// parts of such output usually sit at both ends.
	head := s[:p.MaxBodyChars/2]
	tail := s[len(s)-p.MaxBodyChars/2:]

	return fmt.Sprintf("%s\n\n... [TRUNCATED %d characters - Content side-loaded to outputs/ directory] ...\n\n%s", head, len(s)-p.MaxBodyChars, tail)
}

// truncateJSONFields fits a single JSON document within maxChars while keeping
// the result parseable. It returns ("", false) when the input is not one JSON
// document: malformed, newline-delimited (the caller then tries
// truncateJSONLines), or plain text (the caller falls back to head+tail).
//
// Three strategies, in order:
//
//  1. Re-encode as YAML, losslessly. Do not expect this to shrink JSON much.
//     Measured on this codebase's own payload shape (AppWorld records: a dozen
//     short fields each) YAML lands within 1% of COMPACT JSON -- 9350 vs 9351
//     bytes for 20 records -- and runs ~7% LARGER once the records sit inside a
//     wrapper object. The gain is real but comes from somewhere else: the
//     previous implementation pretty-printed with MarshalIndent, which inflates
//     that same 9351-byte document to 11752, so dropping the indentation
//     recovers ~20% on its own. Anyone tempted to re-derive a "YAML compresses
//     JSON" assumption from the old numbers should measure first; it does not
//     hold for flat records, because `key: value` costs the same as `"key":`.
//  2. Shrink oversized string VALUES (walkAndTruncateStrings), then re-encode.
//     Correct when the bulk is one or two long fields: a file body, a log blob,
//     a base64 payload.
//  3. Keep a leading prefix of the container and state the omission in the YAML
//     header. Correct when the bulk is many small items with no single field
//     worth shrinking.
//
// Strategy 3 exists because strategy 2 cannot help that third shape at all.
// Twenty records of ~1.5K chars each have every individual field far below the
// per-field budget, so each attempt re-serialises to the same size; the previous
// implementation gave up after three attempts and let the caller slice the JSON
// in half, which does not parse. Returning fewer records plus a note that more
// exist is strictly more useful than returning a fragment the model cannot read.
func truncateJSONFields(s string, maxChars int) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false // not JSON -- caller falls back to head+tail
	}
	var obj any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return "", false // not one document (JSONL, or broken) -- caller decides
	}
	originalLen := len(s)

	// Strategy 1: lossless YAML re-encoding.
	if out, ok := renderAsYAML(obj, maxChars, originalLen, 0, 0); ok {
		return out, true
	}

	// Strategy 2: shrink oversized string values, then re-encode.
	perField := maxChars / 10
	if perField < 200 {
		perField = 200
	}
	for attempt := 0; attempt < 3; attempt++ {
		shrunk := walkAndTruncateStrings(obj, perField)
		if out, ok := renderAsYAML(shrunk, maxChars, originalLen, 0, 0); ok {
			return out, true
		}
		perField /= 2
		if perField < 50 {
			break
		}
	}

	// Strategy 3: keep a prefix of the container and state the omission.
	if out, ok := shrinkContainerAsYAML(obj, maxChars, originalLen); ok {
		return out, true
	}

	// Last resort: a small valid document naming the reason, instead of a
	// fragment. Still parseable, so the model can at least reason about it.
	out, err := json.Marshal(map[string]any{
		"_truncated": true,
		"_shown":     0,
		"_hint": fmt.Sprintf(
			"response of %d chars exceeds the %d char budget and cannot be represented; narrow the query",
			len(s), maxChars),
	})
	if err != nil {
		return "", false
	}
	return string(out), true
}

// truncateJSONLines fits newline-delimited JSON within maxChars by keeping whole
// leading lines, and says how many were kept.
//
// This shape needs its own path rather than the plain-text fallback because it
// is not one JSON document: json.Unmarshal rejects it, and a head+tail cut would
// leave every straddling line unparseable while dropping the tail records
// silently. Cutting on line boundaries keeps every retained line a complete,
// individually-parseable record.
//
// Returns ("", false) when the input is not JSONL, so the caller can fall back.
func truncateJSONLines(s string, maxChars int) (string, bool) {
	records := make([]string, 0, 32)
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// Require every non-empty line to be a self-contained JSON object or
		// array. Pretty-printed JSON fails here (its inner lines are neither),
		// which is what keeps this path from stealing documents that the
		// single-document path should have handled.
		if t[0] != '{' && t[0] != '[' {
			return "", false
		}
		if !json.Valid([]byte(t)) {
			return "", false
		}
		records = append(records, t)
	}
	if len(records) < 2 {
		return "", false
	}

	header := func(shown int) string {
		var b strings.Builder
		b.WriteString("# This tool returned newline-delimited JSON (one record per line), shown as JSONL because it is not a single document. ")
		fmt.Fprintf(&b, "%d records, %d chars in total.\n", len(records), len(s))
		if shown < len(records) {
			fmt.Fprintf(&b,
				"# TRUNCATED: showing the first %d of %d records. Request fewer or narrower results to see the rest.\n",
				shown, len(records))
		}
		return b.String()
	}

	// Reserve the fully-truncated header: it is the longest form, so a result
	// built against it cannot overrun the budget regardless of how many lines
	// end up fitting.
	used := len(header(0))
	kept := make([]string, 0, len(records))
	for _, r := range records {
		if used+1+len(r) > maxChars {
			break
		}
		kept = append(kept, r)
		used += 1 + len(r)
	}
	if len(kept) == 0 {
		return "", false
	}
	return header(len(kept)) + strings.Join(kept, "\n") + "\n", true
}

// renderAsYAML encodes v as YAML with a short header, reporting whether the
// result fits in maxChars.
//
// Indent is 2, not yaml.v3's default 4. Nesting is the one shape where YAML
// loses badly to compact JSON, and indentation is pure overhead to a consumer
// that reads the text as tokens: on a wrapped record array the difference is
// 10666 -> 10026 bytes, 6% for a change of one character per level.
//
// shown/total are passed only when records were dropped; both zero means the
// document is complete.
func renderAsYAML(v any, maxChars, originalLen, shown, total int) (string, bool) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		_ = enc.Close()
		return "", false
	}
	if err := enc.Close(); err != nil {
		return "", false
	}
	body := buf.Bytes()
	out := yamlHeader(originalLen, len(body), shown, total) + string(body)
	if len(out) > maxChars {
		return "", false
	}
	return out, true
}

// yamlHeader states what was done to the payload. The model receives text, not a
// parsed tree, so a re-encoded document that is not announced reads as if the
// tool had always returned YAML -- and a truncated one that is not announced
// reads as a complete answer. Both are worth the ~150 bytes.
func yamlHeader(originalLen, renderedLen, shown, total int) string {
	var b strings.Builder
	b.WriteString("# This tool returned JSON; it is shown as YAML to fit the context budget (")
	fmt.Fprintf(&b, "%d -> %d chars). ", originalLen, renderedLen)
	b.WriteString("Same structure and values, only the syntax differs: ")
	b.WriteString("`key: value` for fields, `- ` for list items.\n")
	if total > shown && total > 0 {
		fmt.Fprintf(&b,
			"# TRUNCATED: showing the first %d of %d records. Request fewer or narrower results to see the rest.\n",
			shown, total)
	}
	return b.String()
}

// shrinkContainerAsYAML keeps a leading prefix of a JSON array -- or a sorted
// subset of a JSON object's keys -- and states the omission in the header.
//
// Keeping the prefix rather than sampling is deliberate: record order is often
// meaningful (newest first, as AppWorld's payment-request endpoints do), and a
// caller asking for the next page needs a stable place to resume from.
func shrinkContainerAsYAML(v any, maxChars, originalLen int) (string, bool) {
	switch val := v.(type) {
	case []any:
		return shrinkArrayAsYAML(val, maxChars, originalLen)
	case map[string]any:
		return shrinkObjectAsYAML(val, maxChars, originalLen)
	default:
		return "", false
	}
}

func shrinkArrayAsYAML(val []any, maxChars, originalLen int) (string, bool) {
	total := len(val)
	if total == 0 {
		return "", false
	}

	// Estimate per-element cost from a small sample rather than measuring every
	// prefix, so a 10k-element list costs a couple of marshals, not thousands.
	sample := val
	if len(sample) > 8 {
		sample = sample[:8]
	}
	sampleYAML, err := yaml.Marshal(sample)
	if err != nil {
		return "", false
	}
	perElem := len(sampleYAML) / len(sample)
	if perElem < 1 {
		perElem = 1
	}

	const headerReserve = 400
	keep := (maxChars - headerReserve) / perElem
	if keep > total {
		keep = total
	}
	if keep < 0 {
		keep = 0
	}

	// Elements vary in size, so the estimate can overshoot. Walk down; zero
	// always renders (an empty list plus the header).
	for k := keep; ; k = k * 9 / 10 {
		if out, ok := renderAsYAML(val[:k], maxChars, originalLen, k, total); ok {
			return out, true
		}
		if k == 0 {
			break
		}
	}
	return "", false
}

func shrinkObjectAsYAML(val map[string]any, maxChars, originalLen int) (string, bool) {
	keys := make([]string, 0, len(val))
	for k := range val {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic: map iteration order is randomised

	total := len(keys)
	keep := total
	for {
		subset := make(map[string]any, keep)
		for _, k := range keys[:keep] {
			subset[k] = val[k]
		}
		if out, ok := renderAsYAML(subset, maxChars, originalLen, keep, total); ok {
			return out, true
		}
		if keep == 0 {
			return "", false
		}
		next := keep * 9 / 10
		if next == keep {
			next = keep - 1
		}
		keep = next
	}
}

// walkAndTruncateStrings recursively walks a JSON tree (parsed as any) and
// truncates any string value exceeding maxFieldChars to a head+tail snippet.
// Non-string values (numbers, booleans, null) are left unchanged.
func walkAndTruncateStrings(v any, maxFieldChars int) any {
	switch val := v.(type) {
	case string:
		if len(val) > maxFieldChars {
			half := maxFieldChars / 2
			return val[:half] +
				"...[truncated " + fmt.Sprintf("%d", len(val)-maxFieldChars) + " chars]..." +
				val[len(val)-half:]
		}
		return val
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, vv := range val {
			result[k] = walkAndTruncateStrings(vv, maxFieldChars)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, vv := range val {
			result[i] = walkAndTruncateStrings(vv, maxFieldChars)
		}
		return result
	default:
		return val
	}
}

func (p *OutputProcessor) extractCitations(s string) []string {
	// Simple heuristic: look for things that look like paths or IDs
	var citations []string

	// We'll just do a very basic check for now to avoid too much noise
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Error:") || strings.Contains(line, "FAILED") {
			citations = append(citations, strings.TrimSpace(line))
		}
	}

	if len(citations) > 3 {
		return citations[:3]
	}
	return citations
}
