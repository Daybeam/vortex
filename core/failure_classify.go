package core

import (
	"errors"
	"strings"

	"github.com/daybeam/vortex/providers"
)

// FailureClass is a centralized taxonomy of the kinds of step failure this
// engine already recognizes and handles differently. Before this file, the
// classification logic that decides retry/decision/fail-fast behavior was
// scattered across a chain of strings.Contains(err.Error(), "...") checks
// directly inside the scheduler's execution loop (see the 2026-07-24
// transaction-design discussion). This is a faithful, behavior-preserving
// extraction of that logic into one place, so callers (like per-step
// FailurePolicy overrides -- see schemas/task.go's Step.FailurePolicy) have
// a single, independently-testable source of truth instead of re-deriving
// the same string matches themselves.
type FailureClass string

const (
	// FailureClassRateLimit: a 429-style rate limit response. Handled by
	// retrying with backoff (server Retry-After hint preferred when
	// present, see the 2026-07-21 addendum), capped at step.MaxRetries.
	FailureClassRateLimit FailureClass = "rate_limit"

	// FailureClassBadRequest: a 400-style malformed-request response.
	// Handled by immediately skipping the step (not retried -- retrying an
	// identical malformed request would just fail identically).
	FailureClassBadRequest FailureClass = "bad_request"

	// FailureClassAuthPermission: 401/403/PERMISSION_DENIED. Handled by a
	// circuit breaker: fail fast, no retry (a bad/expired key will not
	// become valid by trying again, so retrying only wastes attempts).
	FailureClassAuthPermission FailureClass = "auth_permission"

	// FailureClassMissingDependency: an environment/tooling prerequisite is
	// absent (e.g. a missing binary a JIT/MCP tool needs). Handled via a
	// decision_required block offering skip/abort/retry_with_skill:shell.
	FailureClassMissingDependency FailureClass = "missing_dependency"

	// FailureClassRoleMissing: the requested role_id does not exist (or
	// dynamic generation failed/is disabled). Handled via a
	// decision_required block offering skip/abort (create_role is
	// intentionally never offered here -- see the 2026-07-05/06 F25
	// security decision to remove it from SubmitDecision entirely).
	FailureClassRoleMissing FailureClass = "role_missing"

	// FailureClassTransient is the catch-all for anything not matching a
	// more specific class above -- handled by the existing fixed/server-
	// hint retry schedule via retryWait.
	FailureClassTransient FailureClass = "transient"

	// ── ODFTP Taxonomy (ADDED 2026-08-30) ─────────────────────────────────

	// FailureClassContextDeficit: subagent explicit signal for missing info.
	FailureClassContextDeficit FailureClass = "context_deficit"

	// FailureClassCapabilityRequired: subagent needs a tool/skill not mounted.
	FailureClassCapabilityRequired FailureClass = "capability_required"

	// FailureClassGenerativeUncertainty: low confidence output on exploratory task.
	FailureClassGenerativeUncertainty FailureClass = "generative_uncertainty"

	// FailureClassContractViolation: output schema mismatch (e.g. invalid JSON).
	FailureClassContractViolation FailureClass = "contract_violation"

	// FailureClassCostOverrun: cost governance abort — cache inefficiency, budget
	// trajectory, or behavior deviation. ADDED (2026-09-14).
	FailureClassCostOverrun FailureClass = "cost_overrun"
)

// classifyError centralizes the failure-classification logic used by the
// scheduler's retry loop. It is a pure function with no side effects,
// deliberately kept separate from what the scheduler *does* about each
// class -- see the call site in scheduler.go's step-error handling.
func classifyError(err error) FailureClass {
	if err == nil {
		return FailureClassTransient
	}

	var rl *providers.RateLimitError
	if errors.As(err, &rl) {
		return FailureClassRateLimit
	}

	var br *providers.BadRequestError
	if errors.As(err, &br) {
		return FailureClassBadRequest
	}

	msg := err.Error()

	// FIX (2026-08-07): the typed *providers.RateLimitError check above only
	// catches rate limits at the point they're first raised inside a
	// provider call. By the time a step's error has been flattened to a
	// plain string (e.g. store/experience.go's RecordTaskCompletion reading
	// step.LastError post-SanitizeError, well downstream of the original
	// typed error), that type information is gone -- so without a string
	// fallback here, the single most common transient-failure class would
	// silently fall through to FailureClassTransient for any caller working
	// from a flattened string rather than the live error chain.
	if strings.Contains(msg, "HTTP 429") || strings.Contains(msg, "RESOURCE_EXHAUSTED") || strings.Contains(strings.ToLower(msg), "rate limit") || strings.Contains(strings.ToLower(msg), "rate_limited") {
		return FailureClassRateLimit
	}

	if strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "PERMISSION_DENIED") || strings.Contains(msg, "HTTP 401") {
		return FailureClassAuthPermission
	}

	if strings.Contains(msg, "missing dependency:") {
		return FailureClassMissingDependency
	}

	if strings.Contains(msg, "role \"") && (strings.Contains(msg, "\" not found") || strings.Contains(msg, "generation failed") || strings.Contains(msg, "generation is disabled")) {
		return FailureClassRoleMissing
	}

	return FailureClassTransient
}
