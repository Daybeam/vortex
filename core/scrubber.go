package core

import (
	"regexp"
)

var (
	// Common secret patterns: API keys, Passwords, etc.
	secretRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),    // OpenAI/Anthropic style
		regexp.MustCompile(`(?i)(AIza[a-zA-Z0-9_\-]{35})`), // Google style
		regexp.MustCompile(`(?i)(api_key|apikey|password|secret|token|credential)["']?\s*[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`),
	}
)

// ScrubSecrets replaces sensitive patterns in a string with [REDACTED].
func ScrubSecrets(text string) string {
	for _, re := range secretRegexes {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// REMOVED (2026-09-20): ScrubMap and ScrubSlice were dead code, redundant with
// pkg/observability.RedactMap which is already wired into logger.go:write()
// (line 282, added 2026-08-28). RedactMap is strictly superior: it checks key
// names (not just values), preserves first 4 chars for debugging, and handles
// nested maps recursively. ScrubSecrets (above) remains live — used by
// context_archive.go:205-207 for flat string scrubbing.
