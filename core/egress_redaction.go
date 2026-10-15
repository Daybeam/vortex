package core

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// ─── Egress Redaction (I5: 出口最小化) ───────────────────────────────────
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

// egressPatterns are compiled once (audit P-3: avoid recompiling per call).
//
// EVAL KILL-SWITCH (2026-10-14): ORCH_DISABLE_EGRESS_REDACTION=1 disables all
// patterns. Rationale — RedactEgress is unconditional in the tool-result path
// (spawner.go:1657/1706, spawner_dispatch.go:119) with no config toggle, and
// its email + JWT rules destroy task-critical data in benchmark domains whose
// *credentials are the task input* (AppWorld: supervisor__show_profile returns
// the login email, app__login returns a JWT access_token). With it on, the
// orchestrator arm is structurally unable to complete such tasks, so any
// A-vs-B delta would measure redaction, not orchestration. Eval-only flag;
// production behavior is unchanged when unset.
var egressPatterns = sync.OnceValue(func() []egressPattern {
	if os.Getenv("ORCH_DISABLE_EGRESS_REDACTION") == "1" {
		return nil
	}
	return []egressPattern{
		{"openai_key", regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`), "sk-[REDACTED]"},
		{"aws_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AKIA[REDACTED]"},
		{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[PRIVATE KEY REDACTED]"},
		{"bearer_token", regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-]{20,}`), "Bearer [REDACTED]"},
		{"jwt", regexp.MustCompile(`eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}`), "[JWT REDACTED]"},
		{"email", regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`), "[EMAIL REDACTED]"},
		{"phone_cn", regexp.MustCompile(`1[3-9]\d{9}`), "[PHONE REDACTED]"},
	}
})

// crossTenantPattern detects OwnerID/tenant references in tool results.
var crossTenantPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:owner|tenant)["\s:]+([a-zA-Z0-9_-]+)`)
})

// RedactEgress strips known secret patterns from text.
// Safe to call on every tool result before it enters the LLM context.
func RedactEgress(text string) string {
	for _, p := range egressPatterns() {
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
