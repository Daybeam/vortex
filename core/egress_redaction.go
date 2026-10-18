package core

import (
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// ─── Egress Redaction (I5: egress minimization) ───────────────────────────────────
//
// Design ref: docs/plan/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §4
//
// Tool results (stdout, file content, API responses) flow back into the
// LLM's context window. If they contain secrets (API keys, PII) or other
// tenants' data, they become ammunition for indirect prompt injection.
//
// RedactEgress strips common secret patterns before results enter the
// turnLoop. RedactEgressWithOwner additionally detects cross-tenant
// OwnerID leakage.

// egressPattern is one redaction rule: a named regex + replacement.
type egressPattern struct {
	name        string
	pattern     *regexp.Regexp
	replacement string
}

// defaultEgressPatterns are compiled once (audit P-3: avoid recompiling per call).
var defaultEgressPatterns = []egressPattern{
	{"openai_key", regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`), "sk-[REDACTED]"},
	{"aws_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AKIA[REDACTED]"},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[PRIVATE KEY REDACTED]"},
	{"bearer_token", regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-]{20,}`), "Bearer [REDACTED]"},
	{"jwt", regexp.MustCompile(`eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}`), "[JWT REDACTED]"},
	{"email", regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`), "[EMAIL REDACTED]"},
	{"phone_cn", regexp.MustCompile(`1[3-9]\d{9}`), "[PHONE REDACTED]"},
	// gap-2 fix (2026-10-18): generic key=value catch-all for custom secret formats
	// in tool results (e.g. DATABASE_URL=postgres://user:pass). Placed last so
	// specific patterns (openai_key, aws_key, etc.) match first.
	{"secret_kv", regexp.MustCompile(`(?i)(api_key|apikey|password|secret|token|credential)["']?\s*[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`), "[SECRET REDACTED]"},
}

// egressRedactionDisabled is an atomic flag toggled by SetEgressRedactionDisabled.
// When true, RedactEgress is a no-op. This replaces the former sync.OnceValue
// that read VORTEX_DISABLE_EGRESS_REDACTION once and cached it permanently.
//
// Backward compat: init() reads the env var so the flag is correct before
// SetEgressRedactionDisabled is called (brief startup window). After startup,
// SetEgressRedactionDisabled is called with the Registry's FeatureFlags value
// (which includes the env override via applySystemDefaults), and on every
// config hot-reload.
//
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md §change 6.
var egressRedactionDisabled atomic.Bool

func init() {
	if os.Getenv("VORTEX_DISABLE_EGRESS_REDACTION") == "1" {
		egressRedactionDisabled.Store(true)
	}
}

// SetEgressRedactionDisabled toggles egress redaction at runtime.
// Called at startup and on config hot-reload to push the
// Registry.System.FeatureFlags.DisableEgressRedaction value.
func SetEgressRedactionDisabled(disabled bool) {
	if disabled {
		log.Printf("WARN: egress redaction DISABLED — tool results will NOT be scrubbed before entering LLM context. Do not use with untrusted relay models.")
	}
	egressRedactionDisabled.Store(disabled)
}

// crossTenantPattern detects OwnerID/tenant references in tool results.
var crossTenantPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:owner|tenant)["\s:]+([a-zA-Z0-9_-]+)`)
})

// RedactEgress strips known secret patterns from text.
// Safe to call on every tool result before it enters the LLM context.
func RedactEgress(text string) string {
	if egressRedactionDisabled.Load() {
		return text
	}
	for _, p := range defaultEgressPatterns {
		text = p.pattern.ReplaceAllString(text, p.replacement)
	}
	return text
}

// RedactEgressWithOwner applies RedactEgress, then checks for cross-tenant
// leakage: if the result contains an OwnerID that is NOT the current owner,
// it is replaced with [OTHER_TENANT:REDACTED].
//
// This is a defense-in-depth measure — it detects potential data leakage
// but cannot prevent all forms (e.g., leaked data without an explicit
// "owner:" prefix).
func RedactEgressWithOwner(text string, currentOwner string) string {
	text = RedactEgress(text)
	if currentOwner == "" {
		return text
	}
	pat := crossTenantPattern()
	for _, m := range pat.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 && m[1] != currentOwner && m[1] != "" {
			text = strings.ReplaceAll(text, m[1], "[OTHER_TENANT:REDACTED]")
		}
	}
	return text
}
