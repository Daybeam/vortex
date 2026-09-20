package core

import (
	"errors"
	"fmt"
	"testing"

	"github.com/daybeam/vortex/providers"
)

func TestClassifyError_Nil(t *testing.T) {
	if got := classifyError(nil); got != FailureClassTransient {
		t.Fatalf("nil error: expected %s, got %s", FailureClassTransient, got)
	}
}

func TestClassifyError_RateLimit(t *testing.T) {
	err := &providers.RateLimitError{Msg: "too many requests", RetryAfter: 30}
	if got := classifyError(err); got != FailureClassRateLimit {
		t.Fatalf("expected %s, got %s", FailureClassRateLimit, got)
	}
}

func TestClassifyError_RateLimit_ThroughWrapping(t *testing.T) {
	// errors.As must unwrap through fmt.Errorf %w wrapping, since callers
	// elsewhere in this codebase often wrap provider errors with context.
	inner := &providers.RateLimitError{Msg: "too many requests"}
	wrapped := fmt.Errorf("provider call failed: %w", inner)
	if got := classifyError(wrapped); got != FailureClassRateLimit {
		t.Fatalf("expected %s for a wrapped RateLimitError, got %s", FailureClassRateLimit, got)
	}
}

func TestClassifyError_BadRequest(t *testing.T) {
	err := &providers.BadRequestError{Msg: "invalid schema"}
	if got := classifyError(err); got != FailureClassBadRequest {
		t.Fatalf("expected %s, got %s", FailureClassBadRequest, got)
	}
}

func TestClassifyError_AuthPermission(t *testing.T) {
	cases := []string{
		"provider error: HTTP 401 Unauthorized",
		"provider error: HTTP 403 Forbidden",
		"rpc error: code = PERMISSION_DENIED desc = ...",
	}
	for _, msg := range cases {
		if got := classifyError(errors.New(msg)); got != FailureClassAuthPermission {
			t.Errorf("message %q: expected %s, got %s", msg, FailureClassAuthPermission, got)
		}
	}
}

func TestClassifyError_MissingDependency(t *testing.T) {
	err := errors.New("missing dependency: D:/bun/bin/bun.exe")
	if got := classifyError(err); got != FailureClassMissingDependency {
		t.Fatalf("expected %s, got %s", FailureClassMissingDependency, got)
	}
}

func TestClassifyError_RoleMissing(t *testing.T) {
	cases := []string{
		`role "nonexistent_role" not found`,
		`role "foo" generation failed: no cookbook source configured`,
		`role "foo" generation is disabled`,
	}
	for _, msg := range cases {
		if got := classifyError(errors.New(msg)); got != FailureClassRoleMissing {
			t.Errorf("message %q: expected %s, got %s", msg, FailureClassRoleMissing, got)
		}
	}
}

func TestClassifyError_RoleMissing_DoesNotFalsePositiveOnUnrelatedRoleMention(t *testing.T) {
	// A message that merely mentions "role" and a quote, without the
	// specific "not found"/"generation failed"/"generation is disabled"
	// phrasing, should NOT be classified as RoleMissing -- this guards
	// against the exact class of over-eager substring-matching bug that
	// has regressed core/tool_router.go three separate times (F11/F15 and
	// the 2026-06-30 addendum's third regression).
	err := errors.New(`role "system_coder" is currently executing step "foo"`)
	if got := classifyError(err); got == FailureClassRoleMissing {
		t.Fatalf("expected NOT %s for an unrelated role-mentioning message, got %s", FailureClassRoleMissing, got)
	}
}

func TestClassifyError_TransientFallback(t *testing.T) {
	err := errors.New("connection reset by peer: EOF")
	if got := classifyError(err); got != FailureClassTransient {
		t.Fatalf("expected %s for an unrecognized error, got %s", FailureClassTransient, got)
	}
}

func TestClassifyError_RateLimit_StringFallback_HTTP429(t *testing.T) {
	// FIX (2026-08-07): a plain string error (e.g. a flattened step.LastError
	// with no live *providers.RateLimitError to unwrap) must still classify
	// as rate-limit, not fall through to Transient.
	err := errors.New("provider call failed: HTTP 429 Too Many Requests")
	if got := classifyError(err); got != FailureClassRateLimit {
		t.Fatalf("expected %s for a flattened HTTP 429 string, got %s", FailureClassRateLimit, got)
	}
}

func TestClassifyError_RateLimit_StringFallback_ResourceExhausted(t *testing.T) {
	err := errors.New(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`)
	if got := classifyError(err); got != FailureClassRateLimit {
		t.Fatalf("expected %s for a flattened RESOURCE_EXHAUSTED string, got %s", FailureClassRateLimit, got)
	}
}

func TestClassifyError_RateLimit_StringFallback_CaseInsensitive(t *testing.T) {
	err := errors.New("upstream said: Rate Limit exceeded, please slow down")
	if got := classifyError(err); got != FailureClassRateLimit {
		t.Fatalf("expected %s for a case-varied 'rate limit' string, got %s", FailureClassRateLimit, got)
	}
}
