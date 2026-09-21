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

// RedactMap recursively redacts sensitive values in a map.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any)
	for k, v := range m {
		kLower := strings.ToLower(k)
		if strings.Contains(kLower, "key") || strings.Contains(kLower, "password") ||
			strings.Contains(kLower, "auth") || strings.Contains(kLower, "token") ||
			strings.Contains(kLower, "secret") {
			if s, ok := v.(string); ok && len(s) > 4 {
				out[k] = s[:4] + "****"
			} else {
				out[k] = "****"
			}
		} else if nm, ok := v.(map[string]any); ok {
			out[k] = RedactMap(nm)
		} else if s, ok := v.(string); ok {
			out[k] = Redact(s)
		} else {
			out[k] = v
		}
	}
	return out
}
