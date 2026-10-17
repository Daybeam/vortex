package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
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
//     (8% of context window × 4 chars/token, clamped [4000, 32000]),
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
// Examples: 8K→4000, 32K→10240, 128K→32000.
func computeDynamicMaxBodyChars(contextWindowTokens int) int {
	if contextWindowTokens <= 0 {
		return defaultMaxBodyChars
	}
	const (
		fraction     = 0.08
		charsPerTok  = 4
		minChars     = 4000
		maxChars     = 32000
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

	// JSON-aware path: preserve structure, truncate field values only.
	// This prevents head+tail from breaking JSON syntax in MCP tool results.
	if truncated, ok := truncateJSONFields(s, p.MaxBodyChars); ok {
		return truncated
	}

	// Fallback: head+tail for non-JSON content (CLI stdout, plain text).
	head := s[:p.MaxBodyChars/2]
	tail := s[len(s)-p.MaxBodyChars/2:]

	return fmt.Sprintf("%s\n\n... [TRUNCATED %d characters - Content side-loaded to outputs/ directory] ...\n\n%s", head, len(s)-p.MaxBodyChars, tail)
}

// truncateJSONFields attempts to truncate large string values within a JSON
// structure while preserving the overall JSON syntax. Returns (result, true)
// on success, or ("", false) if the input is not valid JSON or cannot be
// made to fit within maxChars.
//
// Strategy: parse JSON → walk tree → truncate each string value that exceeds
// a per-field budget (head+tail on the individual string, not the JSON) →
// re-serialize. If the result still exceeds maxChars, retry with a smaller
// per-field budget. After 3 attempts, give up and return false (caller falls
// back to head+tail).
func truncateJSONFields(s string, maxChars int) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false // not JSON
	}
	var obj any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return "", false
	}
	// Per-field budget: maxChars/10 (allows ~10 large fields).
	// Clamped to [200, maxChars] to avoid degenerate values.
	perField := maxChars / 10
	if perField < 200 {
		perField = 200
	}
	for attempt := 0; attempt < 3; attempt++ {
		truncated := walkAndTruncateStrings(obj, perField)
		result, err := json.MarshalIndent(truncated, "", "  ")
		if err != nil {
			return "", false
		}
		if len(string(result)) <= maxChars {
			return string(result), true
		}
		perField /= 2
		if perField < 50 {
			break
		}
	}
	return "", false
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
