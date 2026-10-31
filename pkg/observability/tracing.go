package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

type contextKey string

const (
	TraceIDKey contextKey = "trace_id"
	SpanIDKey  contextKey = "span_id"
)

// NewTraceID generates a 16-byte random hex string (32 chars).
func NewTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSpanID generates an 8-byte random hex string (16 chars).
func NewSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ContextWithTrace returns a new context with TraceID and SpanID.
func ContextWithTrace(ctx context.Context, traceID, spanID string) context.Context {
	if traceID != "" {
		ctx = context.WithValue(ctx, TraceIDKey, traceID)
	}
	if spanID != "" {
		ctx = context.WithValue(ctx, SpanIDKey, spanID)
	}
	return ctx
}

// GetTraceID returns the TraceID from the context or an empty string.
func GetTraceID(ctx context.Context) string {
	if v, ok := ctx.Value(TraceIDKey).(string); ok {
		return v
	}
	return ""
}

// GetSpanID returns the SpanID from the context or an empty string.
func GetSpanID(ctx context.Context) string {
	if v, ok := ctx.Value(SpanIDKey).(string); ok {
		return v
	}
	return ""
}

// StartSpan creates a sub-context with a new SpanID.
func StartSpan(ctx context.Context) (context.Context, string) {
	spanID := NewSpanID()
	return context.WithValue(ctx, SpanIDKey, spanID), spanID
}

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api_key|password|authorization|token|secret)[^:]*[:=]\s*["']?([a-zA-Z0-9\-_./]{4})[a-zA-Z0-9\-_./]+["']?`),
}

// Redact replaces sensitive information in a string with masks.
func Redact(text string) string {
	for _, p := range sensitivePatterns {
		text = p.ReplaceAllString(text, `$1: $2****`)
	}
	return text
}

// sensitiveKeySuffixes are the key shapes that hold an actual credential.
// Matching is on the WHOLE key, not a substring, because substring matching on
// "token" also caught counting fields: prompt_tokens/completion_tokens were
// masked to "****", which silently destroyed the token accounting this engine
// logs on every provider call. A metric named *_tokens is a number, never a
// secret.
//
// Keep this list about credentials. If a future field genuinely carries a
// secret, add its exact name here rather than loosening the match.
var sensitiveKeys = map[string]bool{
	"key":           true,
	"apikey":        true,
	"api_key":       true,
	"access_key":    true,
	"secret":        true,
	"secret_key":    true,
	"client_secret": true,
	"password":      true,
	"passwd":        true,
	"authorization": true,
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"auth_token":    true,
	"bearer":        true,
	"credential":    true,
}

// isSensitiveKey reports whether a detail-map key holds a credential value.
func isSensitiveKey(k string) bool {
	return sensitiveKeys[strings.ToLower(k)]
}

// maskSecret keeps a short prefix for debugging and masks the rest.
func maskSecret(s string) string {
	if len(s) > 4 {
		return s[:4] + "****"
	}
	return "****"
}

// RedactValue redacts a JSON-shaped value. Maps and slices are walked
// recursively: a credential nested in a list or in a map[string]string used to
// pass through untouched (G10, 2026-10-31). Values of other types are returned
// unchanged, so callers holding typed structs must normalize them through
// encoding/json first (see pkg/telemetry.Exporter.Sync).
func RedactValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return Redact(t)
	case map[string]any:
		return RedactMap(t)
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, s := range t {
			if isSensitiveKey(k) {
				out[k] = maskSecret(s)
			} else {
				out[k] = Redact(s)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = RedactValue(e)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = Redact(s)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, m := range t {
			out[i] = RedactMap(m)
		}
		return out
	default:
		return v
	}
}

// RedactMap recursively redacts sensitive values in a map.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if isSensitiveKey(k) {
			if s, ok := v.(string); ok {
				out[k] = maskSecret(s)
			} else {
				out[k] = "****"
			}
			continue
		}
		out[k] = RedactValue(v)
	}
	return out
}
