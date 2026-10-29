package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ── UpstreamSummarizer Interface ───────────────────────────────────────────
//
// UpstreamSummarizer compresses long upstream context into a key-facts
// summary before side-loading in applyRefBasedHandoff. The summary is
// injected to the downstream agent so it can work without reading the
// full artifact file.
//
// See docs/plan/UPSTREAM_CONTEXT_SUMMARIZER_DESIGN.md §2.2.

// UpstreamSummarizer turns a long upstream payload into a concise key-facts
// summary. Implementations must be pure functions (no side effects) and
// safe for concurrent calls.
type UpstreamSummarizer interface {
	Summarize(ctx context.Context, payload []byte, budget int) (string, error)
}

// ── DeterministicFactExtractor ─────────────────────────────────────────────
//
// The default implementation. Uses deterministic rules (no LLM) to extract
// key facts from JSON, Markdown, or code payloads. Zero latency, zero cost,
// zero hallucination, fully testable.
//
// Extraction rules (by priority, see design §2.3):
//  1. Schema overview — JSON object top-level keys + types + value lengths
//  2. Key field matching — known semantic anchors (S2T, DecisionID, Goal:, ...)
//  3. Markdown structured extraction — headings + key paragraph first sentences
//  4. Code symbol index — func/def/class/type signatures

// fidelityAnchors are the semantic signal strings reused from Sieve.VerifyFidelity
// (core/sieve.go) and ContextManager (core/context_manager.go). Extracting
// these fields' full values preserves the critical signals that downstream
// agents need.
var fidelityAnchors = []string{"S2T", "DecisionID", "Goal:", "Final Result:", "$artifact", "$contract"}

// DeterministicFactExtractor extracts key facts from upstream context using
// deterministic rules. It is the default UpstreamSummarizer implementation.
type DeterministicFactExtractor struct{}

// Summarize extracts key facts from payload within the given character budget.
// Returns a structured bullet list of key facts. Never returns an error
// (graceful degradation: if all extraction rules fail, returns a minimal
// schema overview or empty string).
func (d DeterministicFactExtractor) Summarize(ctx context.Context, payload []byte, budget int) (string, error) {
	if len(payload) == 0 {
		return "", nil
	}
	if budget <= 0 {
		budget = 2000
	}

	// Try JSON extraction first (most structured → highest value).
	if summary := extractJSONFacts(payload, budget); summary != "" {
		return truncateSummary(summary, budget), nil
	}

	// Try Markdown extraction (headings + key sentences).
	if summary := extractMarkdownFacts(payload, budget); summary != "" {
		return truncateSummary(summary, budget), nil
	}

	// Try code symbol extraction.
	if summary := extractCodeFacts(payload, budget); summary != "" {
		return truncateSummary(summary, budget), nil
	}

	// Fallback: first N chars as a raw preview.
	preview := string(payload)
	if len(preview) > budget {
		preview = preview[:budget]
	}
	return fmt.Sprintf("[关键事实]\n- (raw preview) %s", preview), nil
}

// ── Rule 1: JSON Schema Overview + Key Field Matching ──────────────────────

// extractJSONFacts tries to parse payload as JSON and extract:
// - Top-level keys with type and truncated value
// - Full values for known semantic anchors (S2T, DecisionID, Goal:, etc.)
func extractJSONFacts(payload []byte, budget int) string {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		// Not a JSON object — try array (extract length + first element schema).
		var arr []any
		if err2 := json.Unmarshal(payload, &arr); err2 == nil {
			return fmt.Sprintf("[关键事实]\n- JSON array: %d items", len(arr))
		}
		return ""
	}

	var facts []string
	keyFieldsFound := 0

	for key, val := range raw {
		typeName, valPreview := describeJSONValue(val, 80)

		// Check if this key matches a known anchor.
		isAnchor := false
		for _, a := range fidelityAnchors {
			if strings.EqualFold(key, a) || strings.Contains(key, a) {
				isAnchor = true
				break
			}
		}

		if isAnchor {
			// Full value for anchor fields (up to 200 chars).
			_, fullVal := describeJSONValue(val, 200)
			facts = append(facts, fmt.Sprintf("- %s = %s", key, fullVal))
			keyFieldsFound++
		} else {
			facts = append(facts, fmt.Sprintf("- %s (%s) = %s", key, typeName, valPreview))
		}
	}

	if len(facts) == 0 {
		return ""
	}

	header := "[关键事实]"
	if keyFieldsFound > 0 {
		header = fmt.Sprintf("[关键事实] (%d anchor fields)", keyFieldsFound)
	}
	result := header + "\n" +GofmtJoinFacts(facts)
	result += fmt.Sprintf("\n- 共 %d 个字段，完整内容见 artifact", len(raw))
	return result
}

// describeJSONValue returns a type name and a truncated value preview.
func describeJSONValue(val any, maxLen int) (string, string) {
	switch v := val.(type) {
	case nil:
		return "null", "null"
	case bool:
		return "bool", fmt.Sprintf("%v", v)
	case float64:
		return "number", fmt.Sprintf("%v", v)
	case string:
		return "string", truncateForSummary(v, maxLen)
	case []any:
		return "array", fmt.Sprintf("%d items", len(v))
	case map[string]any:
		return "object", fmt.Sprintf("%d keys", len(v))
	default:
		return "unknown", truncateForSummary(fmt.Sprintf("%v", v), maxLen)
	}
}

// ── Rule 3: Markdown Structured Extraction ─────────────────────────────────

var (
	markdownHeadingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)`)
	// Key sentence patterns: § references, number+unit, = assignments.
	keySentenceRe = regexp.MustCompile(`(?m)(§\d+\.\d+|[\d,]+\s*(?:天|hours?|items?|rows?|bytes?|MB|GB)|=\s*[\w.-]+)`)
)

// extractMarkdownFacts extracts headings and key sentences from Markdown/text.
func extractMarkdownFacts(payload []byte, budget int) string {
	text := string(payload)
	if !strings.Contains(text, "#") && !strings.Contains(text, "§") {
		return ""
	}

	var facts []string

	// Extract all headings.
	headings := markdownHeadingRe.FindAllStringSubmatch(text, -1)
	for _, h := range headings {
		if len(h) > 1 {
			facts = append(facts, fmt.Sprintf("- # %s", strings.TrimSpace(h[1])))
		}
	}

	// Extract key sentences (§ refs, numeric values, assignments).
	matches := keySentenceRe.FindAllString(text, -1)
	for _, m := range matches {
		facts = append(facts, fmt.Sprintf("- %s", strings.TrimSpace(m)))
	}

	// Also check for anchor strings in the text.
	for _, anchor := range fidelityAnchors {
		if idx := strings.Index(text, anchor); idx >= 0 {
			// Extract the line containing the anchor.
			lineStart := strings.LastIndexByte(text[:idx], '\n') + 1
			lineEnd := strings.IndexByte(text[idx:], '\n')
			if lineEnd < 0 {
				lineEnd = len(text) - idx
			}
			line := strings.TrimSpace(text[lineStart : idx+lineEnd])
			facts = append(facts, fmt.Sprintf("- %s", truncateForSummary(line, 120)))
		}
	}

	if len(facts) == 0 {
		return ""
	}

	// Deduplicate facts.
	seen := make(map[string]bool)
	var unique []string
	for _, f := range facts {
		if !seen[f] {
			seen[f] = true
			unique = append(unique, f)
		}
	}

	return "[关键事实]\n" + GofmtJoinFacts(unique)
}

// ── Rule 4: Code Symbol Index ──────────────────────────────────────────────

var (
	goFuncRe   = regexp.MustCompile(`(?m)^func\s+(\([^)]+\)\s+)?(\w+)\s*\(`)
	goTypeRe   = regexp.MustCompile(`(?m)^type\s+(\w+)\s+`)
	goStructRe = regexp.MustCompile(`(?m)^type\s+(\w+)\s+struct`)
	pyFuncRe   = regexp.MustCompile(`(?m)^def\s+(\w+)\s*\(`)
	pyClassRe  = regexp.MustCompile(`(?m)^class\s+(\w+)`)
	jsFuncRe   = regexp.MustCompile(`(?m)^(?:export\s+)?(?:async\s+)?function\s+(\w+)`)
)

// extractCodeFacts extracts function/type/class signatures from code payloads.
func extractCodeFacts(payload []byte, budget int) string {
	text := string(payload)
	hasCode := strings.Contains(text, "func ") || strings.Contains(text, "def ") ||
		strings.Contains(text, "class ") || strings.Contains(text, "function ") ||
		strings.Contains(text, "type ")
	if !hasCode {
		return ""
	}

	var facts []string

	// Go symbols.
	for _, m := range goFuncRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 2 {
			facts = append(facts, fmt.Sprintf("- func %s()", m[2]))
		}
	}
	for _, m := range goStructRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			facts = append(facts, fmt.Sprintf("- type %s struct", m[1]))
		}
	}
	for _, m := range goTypeRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			facts = append(facts, fmt.Sprintf("- type %s", m[1]))
		}
	}

	// Python symbols.
	for _, m := range pyFuncRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			facts = append(facts, fmt.Sprintf("- def %s()", m[1]))
		}
	}
	for _, m := range pyClassRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			facts = append(facts, fmt.Sprintf("- class %s", m[1]))
		}
	}

	// JS symbols.
	for _, m := range jsFuncRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			facts = append(facts, fmt.Sprintf("- function %s()", m[1]))
		}
	}

	if len(facts) == 0 {
		return ""
	}

	// Deduplicate.
	seen := make(map[string]bool)
	var unique []string
	for _, f := range facts {
		if !seen[f] {
			seen[f] = true
			unique = append(unique, f)
		}
	}

	return fmt.Sprintf("[关键事实] (%d symbols)\n%s", len(unique), GofmtJoinFacts(unique))
}

// ── Helpers ────────────────────────────────────────────────────────────────

// truncateForSummary truncates s to maxLen chars, appending "..." if truncated.
func truncateForSummary(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// truncateSummary truncates the summary to budget chars.
func truncateSummary(summary string, budget int) string {
	if len(summary) <= budget {
		return summary
	}
	return summary[:budget] + "\n... (truncated)"
}

// GofmtJoinFacts joins fact lines with newlines. Named with Gofmt prefix to
// avoid collision with any existing joinFacts helper.
func GofmtJoinFacts(facts []string) string {
	return strings.Join(facts, "\n")
}
