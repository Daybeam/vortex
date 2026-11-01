package redact

import "regexp"

var secretRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),    // OpenAI/Anthropic style
	regexp.MustCompile(`(?i)(AIza[a-zA-Z0-9_\-]{35})`), // Google style
	regexp.MustCompile(`(?i)(api_key|apikey|password|secret|token|credential)["']?\s*[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`),
}

// ScrubSecrets replaces sensitive patterns in a string with [REDACTED].
func ScrubSecrets(text string) string {
	for _, re := range secretRegexes {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
