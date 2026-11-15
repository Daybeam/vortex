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

// D3 (2026-11-11): a spent provider-request allowance is a governance abort,
// not malformed input. It used to arrive as *providers.BadRequestError, so the
// DAG step path skipped the step and the reason read `bad request: {...}`.
func TestClassifyError_BudgetExhausted(t *testing.T) {
	err := &providers.BudgetExhaustedError{
		Msg: `{"error":{"type":"max_agent_turns_exceeded","message":"HarnessBench maximum provider request budget of 40 was reached"}}`,
	}
	if got := classifyError(err); got != FailureClassCostOverrun {
		t.Fatalf("expected %s for an exhausted budget, got %s", FailureClassCostOverrun, got)
	}
}

func TestClassifyError_BudgetExhausted_ThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("step step-1 failed: %w",
		&providers.BudgetExhaustedError{Msg: "maximum provider request budget of 40 was reached"})
	if got := classifyError(wrapped); got != FailureClassCostOverrun {
		t.Fatalf("expected %s for a wrapped budget error, got %s", FailureClassCostOverrun, got)
	}
}

// The end of executeStep re-classifies `errors.New(step.LastError)` — a plain
// string with no live type to unwrap. Without the string fallback an exhausted
// budget would come back as transient there and be retried against a wall.
func TestClassifyError_BudgetExhausted_StringFallback_Flattened(t *testing.T) {
	cases := []string{
		`provider request budget exhausted: {"error":{"type":"max_agent_turns_exceeded"}}`,
		`bad request: {"error":{"type":"max_agent_turns_exceeded","message":"HarnessBench maximum provider request budget of 40 was reached"}}`,
	}
	for _, msg := range cases {
		if got := classifyError(errors.New(msg)); got != FailureClassCostOverrun {
			t.Fatalf("expected %s for flattened %q, got %s", FailureClassCostOverrun, msg, got)
		}
	}
}

// The distinction has to survive in both directions: an ordinary 400 is still
// a bad request, not a budget abort.
func TestClassifyError_BadRequest_IsNotCostOverrun(t *testing.T) {
	err := &providers.BadRequestError{Msg: `{"error":{"message":"messages[0].role must be one of user, assistant"}}`}
	if got := classifyError(err); got != FailureClassBadRequest {
		t.Fatalf("expected %s, got %s", FailureClassBadRequest, got)
	}
}

func TestTriageFailure_EmptyError_FailClosesToCaseOrSpec(t *testing.T) {
	if got := triageFailure(""); got != TriageCaseOrSpec {
		t.Fatalf("empty error: expected %s, got %s", TriageCaseOrSpec, got)
	}
}

func TestTriageFailure_RateLimit_IsInfraNoise(t *testing.T) {
	if got := triageFailure("provider call failed: HTTP 429 Too Many Requests"); got != TriageInfraNoise {
		t.Fatalf("rate limit: expected %s, got %s", TriageInfraNoise, got)
	}
}

func TestTriageFailure_AuthPermission_IsInfraNoise(t *testing.T) {
	if got := triageFailure("provider error: HTTP 401 Unauthorized"); got != TriageInfraNoise {
		t.Fatalf("auth error: expected %s, got %s", TriageInfraNoise, got)
	}
}

func TestTriageFailure_MissingDependency_IsInfraNoise(t *testing.T) {
	if got := triageFailure("missing dependency: D:/bun/bin/bun.exe"); got != TriageInfraNoise {
		t.Fatalf("missing dependency: expected %s, got %s", TriageInfraNoise, got)
	}
}

func TestTriageFailure_RoleMissing_IsCaseOrSpec(t *testing.T) {
	if got := triageFailure(`role "nonexistent_role" not found`); got != TriageCaseOrSpec {
		t.Fatalf("role missing: expected %s, got %s", TriageCaseOrSpec, got)
	}
}

func TestTriageFailure_TransientError_IsInfraNoise(t *testing.T) {
	if got := triageFailure("connection reset by peer: EOF"); got != TriageInfraNoise {
		t.Fatalf("transient error: expected %s, got %s", TriageInfraNoise, got)
	}
}

func TestTriageFailure_CostOverrun_IsSkillDefect(t *testing.T) {
	if got := triageFailure("provider request budget exhausted: max_agent_turns_exceeded"); got != TriageSkillDefect {
		t.Fatalf("cost overrun: expected %s, got %s", TriageSkillDefect, got)
	}
}

func TestTriageFailure_UnrecognizedString_IsInfraNoise(t *testing.T) {
	if got := triageFailure("lua runtime error: attempt to index a nil value"); got != TriageInfraNoise {
		t.Fatalf("unrecognized string defaults to transient→infra_noise, got %s", got)
	}
}
