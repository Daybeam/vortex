package core

import "github.com/daybeam/vortex/core/redact"

// ScrubSecrets replaces sensitive patterns in a string with [REDACTED].
// The implementation lives in core/redact so capability can reuse it.
func ScrubSecrets(text string) string {
	return redact.ScrubSecrets(text)
}
