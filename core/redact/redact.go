package redact

import "regexp"

var secretRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),    // OpenAI/Anthropic style
	regexp.MustCompile(`(?i)(AIza[a-zA-Z0-9_\-]{35})`), // Google style
	regexp.MustCompile(`(?i)(api_key|apikey|password|secret|token|credential)["']?\s*[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`),
	// Telegram bot token, "<bot id>:<secret>". It travels inside a URL path,
	// so the key=value rule above never sees it -- and net/http's url.Error
	// quotes that URL in every transport error, which is how a live token
	// ended up in the channel log next to a timeout.
	regexp.MustCompile(`\d{8,10}:[A-Za-z0-9_\-]{30,}`),
}

// ScrubSecrets replaces sensitive patterns in a string with [REDACTED].
func ScrubSecrets(text string) string {
	for _, re := range secretRegexes {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
